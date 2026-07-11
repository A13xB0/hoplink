package meshtastic

import (
	"bytes"
	"io"
	"testing"
)

func TestEncodeFrame(t *testing.T) {
	got, err := encodeFrame([]byte{1, 2, 3})
	if err != nil {
		t.Fatalf("encodeFrame: %v", err)
	}
	want := []byte{start1, start2, 0, 3, 1, 2, 3}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeFrame = %v, want %v", got, want)
	}
}

func TestEncodeFrame_RejectsOversized(t *testing.T) {
	if _, err := encodeFrame(make([]byte, packetMTU+1)); err == nil {
		t.Fatal("expected error for a frame exceeding packetMTU")
	}
}

func TestWakeSequence(t *testing.T) {
	got := wakeSequence()
	if len(got) != wakeByteCount {
		t.Fatalf("len(wakeSequence()) = %d, want %d", len(got), wakeByteCount)
	}
	for i, b := range got {
		if b != start2 {
			t.Fatalf("wakeSequence()[%d] = %#x, want %#x", i, b, start2)
		}
	}
}

func TestFrameReader_SingleFrame(t *testing.T) {
	raw := []byte{start1, start2, 0, 3, 'a', 'b', 'c'}
	fr := newFrameReader(bytes.NewReader(raw))
	got, err := fr.readFrame()
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if !bytes.Equal(got, []byte("abc")) {
		t.Errorf("got %q, want %q", got, "abc")
	}
}

func TestFrameReader_MultipleFrames(t *testing.T) {
	raw := []byte{
		start1, start2, 0, 2, 'h', 'i',
		start1, start2, 0, 3, 'b', 'y', 'e',
	}
	fr := newFrameReader(bytes.NewReader(raw))

	f1, err := fr.readFrame()
	if err != nil {
		t.Fatalf("readFrame 1: %v", err)
	}
	if !bytes.Equal(f1, []byte("hi")) {
		t.Errorf("frame 1 = %q, want %q", f1, "hi")
	}

	f2, err := fr.readFrame()
	if err != nil {
		t.Fatalf("readFrame 2: %v", err)
	}
	if !bytes.Equal(f2, []byte("bye")) {
		t.Errorf("frame 2 = %q, want %q", f2, "bye")
	}

	if _, err := fr.readFrame(); err != io.EOF {
		t.Errorf("expected io.EOF after last frame, got %v", err)
	}
}

func TestFrameReader_SkipsWakeSequenceAndNoise(t *testing.T) {
	raw := append(wakeSequence(), []byte{start1, start2, 0, 2, 'o', 'k'}...)
	fr := newFrameReader(bytes.NewReader(raw))
	got, err := fr.readFrame()
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if !bytes.Equal(got, []byte("ok")) {
		t.Errorf("got %q, want %q", got, "ok")
	}
}

func TestFrameReader_ResyncsOnStart1WithoutStart2(t *testing.T) {
	// A stray start1 byte not followed by start2 must not swallow a real
	// frame's start1 that immediately follows it.
	raw := []byte{start1, 0xAA} // stray, not a real header
	raw = append(raw, start1, start2, 0, 2, 'o', 'k')
	fr := newFrameReader(bytes.NewReader(raw))
	got, err := fr.readFrame()
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if !bytes.Equal(got, []byte("ok")) {
		t.Errorf("got %q, want %q", got, "ok")
	}
}

func TestFrameReader_ResyncsOnImplausibleLength(t *testing.T) {
	raw := []byte{start1, start2, 0xFF, 0xFF} // implausible length
	raw = append(raw, start1, start2, 0, 2, 'o', 'k')
	fr := newFrameReader(bytes.NewReader(raw))
	got, err := fr.readFrame()
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if !bytes.Equal(got, []byte("ok")) {
		t.Errorf("got %q, want %q", got, "ok")
	}
}
