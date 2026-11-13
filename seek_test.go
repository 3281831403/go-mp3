// Copyright 2026 The go-mp3 Authors
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
	"bytes"
	"io"
	"io/ioutil"
	"math/rand"
	"testing"
)

// decodeReference decodes the whole stream sequentially and returns every
// output byte. It is the ground truth for Seek byte-identity checks.
func decodeReference(t *testing.T, src []byte) []byte {
	t.Helper()
	d, err := NewDecoder(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	out, err := ioutil.ReadAll(d)
	if err != nil {
		t.Fatalf("sequential decode: %v", err)
	}
	return out
}

func newSeekingDecoder(t *testing.T, src []byte) *Decoder {
	t.Helper()
	d, err := NewDecoder(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	return d
}

// TestSeekByteIdentityAtFrameBoundaries verifies that, after a Seek to every
// frame boundary, the following bytes are identical to a from-the-start
// sequential decode at the same offset.
func TestSeekByteIdentityAtFrameBoundaries(t *testing.T) {
	for _, name := range []string{"example/classic.mp3", "example/mpeg2.mp3"} {
		t.Run(name, func(t *testing.T) {
			src, err := ioutil.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			ref := decodeReference(t, src)
			d := newSeekingDecoder(t, src)

			f := 0
			for off := int64(0); off < d.Length(); off += d.bytesPerFrame {
				if p, err := d.Seek(off, io.SeekStart); err != nil || p != off {
					t.Fatalf("frame %d: Seek(%d) = (%d, %v)", f, off, p, err)
				}
				// Read a couple of frames worth of data so the reservoir /
				// overlap-add state of more than one frame is exercised.
				n := int(2 * d.bytesPerFrame)
				if int64(n) > d.Length()-off {
					n = int(d.Length() - off)
				}
				got := make([]byte, n)
				if _, err := io.ReadFull(d, got); err != nil {
					t.Fatalf("frame %d: ReadFull at %d: %v", f, off, err)
				}
				if !bytes.Equal(got, ref[off:off+int64(n)]) {
					at := 0
					for at < n && got[at] == ref[off+int64(at)] {
						at++
					}
					t.Fatalf("frame %d offset %d: byte mismatch at +%d", f, off, at)
				}
				f++
			}
		})
	}
}

// TestSeekByteIdentityArbitraryOffsets checks offsets that are not frame
// aligned, including offsets that are not sample aligned.
func TestSeekByteIdentityArbitraryOffsets(t *testing.T) {
	src, err := ioutil.ReadFile("example/classic.mp3")
	if err != nil {
		t.Fatal(err)
	}
	ref := decodeReference(t, src)
	d := newSeekingDecoder(t, src)

	offsets := []int64{1, 2, 3, 5, 7, 4095, 4096, 4097}
	// Add offsets spread across the whole stream.
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 32; i++ {
		offsets = append(offsets, r.Int63n(d.Length()))
	}
	for _, off := range offsets {
		if p, err := d.Seek(off, io.SeekStart); err != nil || p != off {
			t.Fatalf("Seek(%d) = (%d, %v)", off, p, err)
		}
		n := int64(3 * d.bytesPerFrame)
		if n > d.Length()-off {
			n = d.Length() - off
		}
		got := make([]byte, n)
		if _, err := io.ReadFull(d, got); err != nil {
			t.Fatalf("ReadFull at %d: %v", off, err)
		}
		if !bytes.Equal(got, ref[off:off+n]) {
			t.Fatalf("offset %d: got %x..., want %x...", off, got[:min64(16, n)], ref[off:off+min64(16, n)])
		}
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// TestSeekThenSustainedRead verifies that after a single Seek the decoder can
// be read forward for many frames and every byte still matches the sequential
// decode. This proves the rebuilt reservoir and overlap-add state keep being
// propagated correctly long after the seek window is left behind.
func TestSeekThenSustainedRead(t *testing.T) {
	for _, name := range []string{"example/classic.mp3", "example/mpeg2.mp3"} {
		t.Run(name, func(t *testing.T) {
			src, _ := ioutil.ReadFile(name)
			ref := decodeReference(t, src)
			d := newSeekingDecoder(t, src)

			// Seek a quarter of the way in, then read 64 frames sequentially.
			off := d.Length() / 4
			off -= off % d.bytesPerFrame
			if _, err := d.Seek(off, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			n := 64 * d.bytesPerFrame
			if n > d.Length()-off {
				n = d.Length() - off
			}
			got := make([]byte, n)
			if _, err := io.ReadFull(d, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, ref[off:off+n]) {
				t.Fatal("sustained read after seek mismatch")
			}
		})
	}
}

// TestSeekRepeatedBackAndForward exercises the same Decoder being seeked in
// arbitrary directions, which previously produced unstable output.
func TestSeekRepeatedBackAndForward(t *testing.T) {
	src, err := ioutil.ReadFile("example/classic.mp3")
	if err != nil {
		t.Fatal(err)
	}
	ref := decodeReference(t, src)
	d := newSeekingDecoder(t, src)
	r := rand.New(rand.NewSource(2))

	for i := 0; i < 64; i++ {
		off := r.Int63n(d.Length())
		if _, err := d.Seek(off, io.SeekStart); err != nil {
			t.Fatalf("Seek(%d): %v", off, err)
		}
		n := int64(d.bytesPerFrame)
		if n > d.Length()-off {
			n = d.Length() - off
		}
		got := make([]byte, n)
		if _, err := io.ReadFull(d, got); err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		if !bytes.Equal(got, ref[off:off+n]) {
			t.Fatalf("iteration %d offset %d: mismatch", i, off)
		}
	}
}

// TestSeekWhences checks SeekCurrent and SeekEnd against SeekStart semantics.
func TestSeekWhences(t *testing.T) {
	src, _ := ioutil.ReadFile("example/classic.mp3")
	ref := decodeReference(t, src)
	d := newSeekingDecoder(t, src)

	mid := d.Length() / 2
	if _, err := d.Seek(mid, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	// SeekCurrent backwards.
	if p, err := d.Seek(-100, io.SeekCurrent); err != nil || p != mid-100 {
		t.Fatalf("SeekCurrent: p=%d err=%v", p, err)
	}
	// SeekEnd.
	if p, err := d.Seek(-200, io.SeekEnd); err != nil || p != d.Length()-200 {
		t.Fatalf("SeekEnd: p=%d err=%v", p, err)
	}
	off := d.Length() - 200
	got := make([]byte, 200)
	if _, err := io.ReadFull(d, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, ref[off:]) {
		t.Fatal("SeekEnd read mismatch")
	}
	// SeekCurrent with zero offset just reports the position.
	posBefore := d.pos
	if p, err := d.Seek(0, io.SeekCurrent); err != nil || p != posBefore {
		t.Fatalf("SeekCurrent(0): p=%d err=%v want %d", p, err, posBefore)
	}
	// Invalid whence.
	if _, err := d.Seek(0, 42); err == nil {
		t.Fatal("invalid whence: expected error")
	}
}

// TestSeekInvalidOffsets ensures bad arguments produce errors instead of
// panics.
func TestSeekInvalidOffsets(t *testing.T) {
	src, _ := ioutil.ReadFile("example/classic.mp3")
	d := newSeekingDecoder(t, src)

	cases := []struct {
		name   string
		offset int64
		whence int
	}{
		{"negative start", -1, io.SeekStart},
		{"negative current", -d.Length(), io.SeekCurrent},
		{"one past end", 1, io.SeekEnd},
		{"far past end", d.Length() + 1, io.SeekStart},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked: %v", r)
				}
			}()
			if _, err := d.Seek(c.offset, c.whence); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// TestSeekEndIsReadableEOF checks that seeking to Length succeeds and the next
// Read reports io.EOF (previously Seek to Length returned an error).
func TestSeekEndIsReadableEOF(t *testing.T) {
	src, _ := ioutil.ReadFile("example/classic.mp3")
	d := newSeekingDecoder(t, src)

	p, err := d.Seek(d.Length(), io.SeekStart)
	if err != nil {
		t.Fatalf("Seek to Length: %v", err)
	}
	if p != d.Length() {
		t.Fatalf("position = %d, want %d", p, d.Length())
	}
	n, err := d.Read(make([]byte, 4))
	if n != 0 || err != io.EOF {
		t.Fatalf("Read at end = (%d, %v), want (0, EOF)", n, err)
	}
	// Reading up to the last byte still works and matches the reference.
	ref := decodeReference(t, src)
	if _, err := d.Seek(d.Length()-8, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail := make([]byte, 8)
	if _, err := io.ReadFull(d, tail); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tail, ref[len(ref)-8:]) {
		t.Fatal("last bytes mismatch")
	}
	if n, err := d.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("Read after tail = (%d, %v), want (0, EOF)", n, err)
	}
}

// onlyReader wraps a reader so the Seek method of the underlying type is hidden.
type onlyReader struct{ r io.Reader }

func (o *onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

// TestSeekNonSeekableSource returns an error rather than panicking.
func TestSeekNonSeekableSource(t *testing.T) {
	src, _ := ioutil.ReadFile("example/classic.mp3")
	d, err := NewDecoder(&onlyReader{r: bytes.NewReader(src)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Length() != -1 {
		t.Fatalf("Length = %d, want -1 for non-seekable source", d.Length())
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	if _, err := d.Seek(0, io.SeekStart); err == nil {
		t.Fatal("expected error for non-seekable source")
	}
	// Reads keep working.
	buf := make([]byte, 128)
	if _, err := io.ReadFull(d, buf); err != nil {
		t.Fatalf("read after failed seek: %v", err)
	}
}

// TestTruncatedLength matches the actually decodable bytes: the incomplete
// last frame is neither counted in Length nor reachable through Seek.
func TestTruncatedLength(t *testing.T) {
	src, _ := ioutil.ReadFile("example/classic.mp3")
	trunc := make([]byte, len(src)-300)
	copy(trunc, src)

	d := newSeekingDecoder(t, trunc)
	out, err := ioutil.ReadAll(d)
	if err != nil {
		t.Fatalf("decoding truncated file: %v", err)
	}
	if int64(len(out)) != d.Length() {
		t.Fatalf("Length = %d, actual decodable = %d", d.Length(), len(out))
	}
	// The full file decodes one frame more.
	full := newSeekingDecoder(t, src)
	if full.Length() != d.Length()+d.bytesPerFrame {
		t.Fatalf("truncated Length = %d, full = %d, want one frame difference",
			d.Length(), full.Length())
	}
	// Every decodable offset of the truncated file matches the full decode.
	ref := decodeReference(t, src)
	for _, off := range []int64{0, d.Length() / 2, d.Length() - d.bytesPerFrame} {
		if _, err := d.Seek(off, io.SeekStart); err != nil {
			t.Fatalf("Seek(%d): %v", off, err)
		}
		n := d.Length() - off
		got := make([]byte, n)
		if _, err := io.ReadFull(d, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, ref[off:off+n]) {
			t.Fatalf("truncated decode mismatch at %d", off)
		}
	}
}

// TestSeekCostIsBounded verifies the number of frames decoded per Seek does
// not grow with the seek distance: seeks near the start and near the end must
// rebuild the same small number of frames.
func TestSeekCostIsBounded(t *testing.T) {
	for _, name := range []string{"example/classic.mp3", "example/mpeg2.mp3"} {
		t.Run(name, func(t *testing.T) {
			src, _ := ioutil.ReadFile(name)
			d := newSeekingDecoder(t, src)

			windowAt := func(frameIdx int) int {
				f := frameIdx
				start := f - seekPredecessorFrames
				if start < 0 {
					start = 0
				}
				return f - start + 1
			}
			nearStart := windowAt(100)
			nearEnd := windowAt(len(d.frameStarts) - 100)
			if nearStart != nearEnd {
				t.Fatalf("seek window varies with distance: %d vs %d", nearStart, nearEnd)
			}
			if nearStart > seekPredecessorFrames+1 {
				t.Fatalf("seek window %d exceeds the fixed bound %d",
					nearStart, seekPredecessorFrames+1)
			}
			t.Logf("frames decoded per seek: %d (of %d total)", nearStart, len(d.frameStarts))
		})
	}
}
