package meshcore

import (
	"bytes"
	"io"
	"testing"
)

func TestEncodeFrame(t *testing.T) {
	got := EncodeFrame([]byte{1, 2, 3})
	want := []byte{0x3C, 3, 0, 1, 2, 3}
	if !bytes.Equal(got, want) {
		t.Errorf("EncodeFrame = %v, want %v", got, want)
	}
}

func TestFrameReader_SingleFrame(t *testing.T) {
	raw := []byte{0x3E, 3, 0, 'a', 'b', 'c'}
	fr := NewFrameReader(bytes.NewReader(raw))
	got, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(got, []byte("abc")) {
		t.Errorf("got %q, want %q", got, "abc")
	}
}

func TestFrameReader_MultipleFramesAndTrailingPartial(t *testing.T) {
	raw := []byte{
		0x3E, 2, 0, 'h', 'i',
		0x3E, 3, 0, 'b', 'y', 'e',
	}
	fr := NewFrameReader(bytes.NewReader(raw))

	f1, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame 1: %v", err)
	}
	if !bytes.Equal(f1, []byte("hi")) {
		t.Errorf("frame 1 = %q, want %q", f1, "hi")
	}

	f2, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame 2: %v", err)
	}
	if !bytes.Equal(f2, []byte("bye")) {
		t.Errorf("frame 2 = %q, want %q", f2, "bye")
	}

	if _, err := fr.ReadFrame(); err != io.EOF {
		t.Errorf("expected io.EOF after last frame, got %v", err)
	}
}

func TestFrameReader_SkipsBootBannerNoise(t *testing.T) {
	raw := append([]byte("garbage boot banner text\r\n"), []byte{0x3E, 2, 0, 'o', 'k'}...)
	fr := NewFrameReader(bytes.NewReader(raw))
	got, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(got, []byte("ok")) {
		t.Errorf("got %q, want %q", got, "ok")
	}
}

func TestFrameReader_ResyncsOnImplausibleLength(t *testing.T) {
	// A stray 0x3E inside noise, followed by a length that would overflow
	// maxFrameLength, then a real frame.
	raw := []byte{0x3E, 0xFF, 0xFF} // implausible length (65535)
	raw = append(raw, 0x3E, 2, 0, 'o', 'k')
	fr := NewFrameReader(bytes.NewReader(raw))
	got, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(got, []byte("ok")) {
		t.Errorf("got %q, want %q", got, "ok")
	}
}
