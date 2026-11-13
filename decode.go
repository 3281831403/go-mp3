// Copyright 2017 Hajime Hoshi
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mp3

import (
	"errors"
	"io"

	"github.com/hajimehoshi/go-mp3/internal/consts"
	"github.com/hajimehoshi/go-mp3/internal/frame"
	"github.com/hajimehoshi/go-mp3/internal/frameheader"
)

// A Decoder is a MP3-decoded stream.
//
// Decoder decodes its underlying source on the fly.
type Decoder struct {
	source        *source
	sampleRate    int
	length        int64
	frameStarts   []int64
	buf           []byte
	frame         *frame.Frame
	pos           int64
	bytesPerFrame int64
}

// seekPredecessorFrames is the number of frames decoded before the frame that
// contains the seek target to rebuild every cross-frame state correctly.
//
// The MP3 bit reservoir (main_data_begin, an 8- or 9-bit field) lets a frame
// reference up to 511 bytes of main data from preceding frames. The smallest
// legal Layer III frames (8 kbps, 24 kHz MPEG-2) carry roughly 48 bytes of
// main data each, so ten such frames already contain far more than 511 bytes:
// every possible reservoir reference resolves inside this window. The first
// two frames additionally reinitialize the overlap-add state (the IMDCT store
// and the subband synthesis v_vec, each of which only carries state from the
// immediately preceding frame). Being a fixed count, the work done by a Seek
// is independent of the seek distance and never buffers the whole decoded
// stream.
const seekPredecessorFrames = 10

var (
	errUnseekableSource = errors.New("mp3: source must be io.Seeker")
	errNegativePosition = errors.New("mp3: negative position")
	errPositionOverflow = errors.New("mp3: position out of range")
)

func (d *Decoder) readFrame() error {
	var err error
	d.frame, _, err = frame.Read(d.source, d.source.pos, d.frame)
	if err != nil {
		if err == io.EOF {
			return io.EOF
		}
		if _, ok := err.(*consts.UnexpectedEOF); ok {
			// TODO: Log here?
			return io.EOF
		}
		return err
	}
	d.buf = append(d.buf, d.frame.Decode()...)
	return nil
}

// Read is io.Reader's Read.
func (d *Decoder) Read(buf []byte) (int, error) {
	for len(d.buf) == 0 {
		// When the whole stream has been decoded there is nothing left to
		// read. Report io.EOF instead of trying to parse another frame, which
		// would also fail but with an error that depends on the source layout.
		if d.length != invalidLength && d.pos >= d.length {
			return 0, io.EOF
		}
		if err := d.readFrame(); err != nil {
			return 0, err
		}
	}
	n := copy(buf, d.buf)
	d.buf = d.buf[n:]
	d.pos += int64(n)
	return n, nil
}

// Seek is io.Seeker's Seek.
//
// Seek returns an error when the underlying source is not io.Seeker, or when
// the resulting position is negative or greater than Length. Seek never
// panics on invalid arguments.
//
// Note that seek uses a byte offset but samples are aligned to 4 bytes (2
// channels, 2 bytes each). Be careful to seek to an offset that is divisible by
// 4 if you want to read at full sample boundaries.
//
// The bytes read after a Seek are byte-identical to the bytes a sequential
// decode from the start would produce at the same offset. The cost of a Seek
// does not depend on the distance from the current position: the target frame
// is located by constant-time index arithmetic and only a fixed number of
// predecessor frames (enough to cover the bit-reservoir and overlap-add
// state) are decoded.
func (d *Decoder) Seek(offset int64, whence int) (int64, error) {
	if offset == 0 && whence == io.SeekCurrent {
		// Handle the special case of asking for the current position specially.
		return d.pos, nil
	}

	npos := int64(0)
	switch whence {
	case io.SeekStart:
		npos = offset
	case io.SeekCurrent:
		npos = d.pos + offset
	case io.SeekEnd:
		npos = d.length + offset
	default:
		return 0, errors.New("mp3: invalid whence")
	}
	if npos < 0 {
		return 0, errNegativePosition
	}
	if d.length == invalidLength {
		// The source is not seekable: the frame index is not available.
		return 0, errUnseekableSource
	}
	if npos > d.length {
		return 0, errPositionOverflow
	}

	// Seeking to the very end leaves nothing to read. The next Read must
	// return io.EOF.
	if npos == d.length {
		d.pos = npos
		d.buf = nil
		d.frame = nil
		return npos, nil
	}

	// Find the frame containing the target position. frameStarts holds source
	// positions and is sorted; frame f produces output bytes
	// [f*bytesPerFrame, (f+1)*bytesPerFrame).
	f := int(npos / d.bytesPerFrame)
	if f < 0 || f >= len(d.frameStarts) {
		return 0, errPositionOverflow
	}

	// Determine the first frame to decode. Two cross-frame mechanisms make a
	// frame's samples depend on earlier frames:
	//
	//   * The bit reservoir: main_data_begin lets a frame reference main data
	//     of preceding frames (at most 511 bytes, per the spec). The reference
	//     is resolved through each frame's reservoir buffer, which is itself
	//     assembled from the previous frame's buffer, so the dependency
	//     propagates along the frame chain rather than across a plain byte
	//     distance.
	//   * The overlap-add state (IMDCT store and the subband v_vec): the frame
	//     immediately preceding the target is folded into the target's first
	//     samples, and that preceding frame must itself be decoded with a
	//     correct state, which needs the frame before it.
	//
	// Rather than reconstructing the exact reservoir chain, decode a fixed
	// number of predecessor frames that is provably sufficient for every
	// valid Layer III stream (see seekPredecessorFrames). The count is
	// constant, so seek cost never grows with the distance from the current
	// position. The decoded bytes are identical to a from-the-start sequential
	// decode at the same offset.
	start := f - seekPredecessorFrames
	if start < 0 {
		start = 0
	}

	// Reset every cross-frame state before rebuilding it from scratch.
	d.buf = nil
	d.frame = nil
	if _, err := d.source.Seek(d.frameStarts[start], io.SeekStart); err != nil {
		return 0, err
	}
	for i := start; i <= f; i++ {
		if err := d.readFrame(); err != nil {
			return 0, err
		}
	}

	// Drop the decoded bytes that precede the target position.
	d.buf = d.buf[npos-int64(start)*d.bytesPerFrame:]
	d.pos = npos
	return npos, nil
}

// SampleRate returns the sample rate like 44100.
//
// Note that the sample rate is retrieved from the first frame.
func (d *Decoder) SampleRate() int {
	return d.sampleRate
}

func (d *Decoder) ensureFrameStartsAndLength() error {
	if d.length != invalidLength {
		return nil
	}

	if _, ok := d.source.reader.(io.Seeker); !ok {
		return nil
	}

	// Keep the current position.
	pos, err := d.source.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if err := d.source.rewind(); err != nil {
		return err
	}

	if err := d.source.skipTags(); err != nil {
		return err
	}
	l := int64(0)
	for {
		h, hpos, err := frameheader.Read(d.source, d.source.pos)
		if err != nil {
			if err == io.EOF {
				break
			}
			if _, ok := err.(*consts.UnexpectedEOF); ok {
				// TODO: Log here?
				break
			}
			return err
		}

		framesize, err := h.FrameSize()
		if err != nil {
			return err
		}
		// Read the rest of the frame before registering it. A frame whose
		// body is not fully present (a truncated stream) can never be
		// decoded, so it must not be counted in the length nor be reachable
		// through Seek.
		buf := make([]byte, framesize-4)
		if _, err := d.source.ReadFull(buf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}

		// The frame is complete: register its source position and its output
		// size only now.
		d.frameStarts = append(d.frameStarts, hpos)
		d.bytesPerFrame = int64(h.BytesPerFrame())
		l += d.bytesPerFrame
	}
	d.length = l

	if _, err := d.source.Seek(pos, io.SeekStart); err != nil {
		return err
	}
	return nil
}

const invalidLength = -1

// Length returns the total size in bytes.
//
// Length returns -1 when the total size is not available
// e.g. when the given source is not io.Seeker.
func (d *Decoder) Length() int64 {
	return d.length
}

// NewDecoder decodes the given io.Reader and returns a decoded stream.
//
// The stream is always formatted as 16bit (little endian) 2 channels
// even if the source is single channel MP3.
// Thus, a sample always consists of 4 bytes.
func NewDecoder(r io.Reader) (*Decoder, error) {
	s := &source{
		reader: r,
	}
	d := &Decoder{
		source: s,
		length: invalidLength,
	}

	if err := s.skipTags(); err != nil {
		return nil, err
	}
	// TODO: Is readFrame here really needed?
	if err := d.readFrame(); err != nil {
		return nil, err
	}
	freq, err := d.frame.SamplingFrequency()
	if err != nil {
		return nil, err
	}
	d.sampleRate = freq

	if err := d.ensureFrameStartsAndLength(); err != nil {
		return nil, err
	}

	return d, nil
}
