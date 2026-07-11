package meshcore

import (
	"bufio"
	"encoding/binary"
	"io"
)

// Serial framing markers for the companion protocol over USB-serial/TCP.
// App -> radio: '<' (0x3C) + u16-LE length + frame bytes.
// Radio -> app: '>' (0x3E) + u16-LE length + frame bytes.
const (
	frameMarkerToRadio   = 0x3C // '<'
	frameMarkerFromRadio = 0x3E // '>'

	// maxFrameLength guards against runaway reads if the stream desyncs;
	// generously above the real MAX_FRAME_SIZE (176).
	maxFrameLength = 1024
)

// EncodeFrame wraps a companion frame for transmission to the radio:
// '<' + u16-LE length + frame.
func EncodeFrame(frame []byte) []byte {
	return encodeFrameWithMarker(frame, frameMarkerToRadio)
}

// encodeFrameWithMarker is used by tests to encode frames in the radio ->
// app direction (marked '>') when simulating the radio side.
func encodeFrameWithMarker(frame []byte, marker byte) []byte {
	out := make([]byte, 3+len(frame))
	out[0] = marker
	binary.LittleEndian.PutUint16(out[1:3], uint16(len(frame)))
	copy(out[3:], frame)
	return out
}

// FrameReader incrementally parses a companion-protocol byte stream,
// resyncing on its marker byte so boot banners or stray bytes are skipped
// safely (mirrors MeshCIM's SerialFrameAccumulator).
type FrameReader struct {
	r      *bufio.Reader
	marker byte
}

// NewFrameReader wraps r for frame-at-a-time reading of the radio -> app
// direction (frames marked with '>').
func NewFrameReader(r io.Reader) *FrameReader {
	return newFrameReaderWithMarker(r, frameMarkerFromRadio)
}

// newFrameReaderWithMarker is used by tests to read the app -> radio
// direction (frames marked with '<') when simulating the radio side of the
// connection.
func newFrameReaderWithMarker(r io.Reader, marker byte) *FrameReader {
	return &FrameReader{r: bufio.NewReaderSize(r, 4096), marker: marker}
}

// ReadFrame blocks until one complete companion frame (marker/length
// stripped) is available, or returns an error (including io.EOF) if the
// underlying reader fails.
func (fr *FrameReader) ReadFrame() ([]byte, error) {
	for {
		b, err := fr.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b != fr.marker {
			continue
		}

		var hdr [2]byte
		if _, err := io.ReadFull(fr.r, hdr[:]); err != nil {
			return nil, err
		}
		length := binary.LittleEndian.Uint16(hdr[:])
		if length == 0 || length > maxFrameLength {
			// Implausible length: this '>' was not a real frame marker.
			// Resync by continuing to scan rather than aborting the stream.
			continue
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(fr.r, body); err != nil {
			return nil, err
		}
		return body, nil
	}
}
