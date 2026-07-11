// Package meshtastic implements the Meshtastic device "client API" stream
// protocol over TCP: framing, the want_config handshake, channel-name
// resolution against the attached device's own configured channel slots,
// a node database for resolving sender names, and text-message send/receive.
//
// Unlike MeshCore's raw-packet approach, Meshtastic's client API only lets
// an app address channels by local slot index (0-7) on the attached
// device — the device itself performs the channel's AES-CTR
// encryption/decryption, not the app. So a channel must already exist as a
// slot on the physically-attached device (configured via the official
// Meshtastic app/CLI) for this package to send/receive on it.
//
// Reference: https://meshtastic.org/docs/development/device/client-api/
// and github.com/meshnet-gophers/meshtastic-go (MIT), whose generated
// protobuf types (from Meshtastic's own .proto schema) this package
// depends on, and whose transport/stream_conn.go and radio/aes.go were
// read to cross-check framing and crypto behavior against this package's
// own from-scratch implementation.
package meshtastic

import (
	"bufio"
	"encoding/binary"
	"io"
)

// Stream framing magic bytes and limits (client-api).
const (
	start1 = 0x94
	start2 = 0xC3

	// packetMTU is the maximum protobuf message size the stream protocol
	// allows within its 2-byte length header.
	packetMTU = 512

	// wakeByteCount is how many start2 bytes are sent once after connecting
	// to wake a sleeping device before any real message is sent.
	wakeByteCount = 32
)

// frameReader incrementally parses the stream protocol's
// start1/start2/length-prefixed frames, resyncing on the magic sequence so
// any boot/debug text the device writes is skipped safely.
type frameReader struct {
	r *bufio.Reader
}

func newFrameReader(r io.Reader) *frameReader {
	return &frameReader{r: bufio.NewReaderSize(r, 4096)}
}

// readFrame blocks until one complete protobuf-message frame (header
// stripped) is available, or returns an error (including io.EOF).
func (fr *frameReader) readFrame() ([]byte, error) {
	for {
		b, err := fr.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b != start1 {
			continue
		}
		b2, err := fr.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b2 != start2 {
			// Not a real header; the byte we just consumed could itself be
			// a start1, so don't discard it — re-scan from here.
			if b2 == start1 {
				if err := fr.r.UnreadByte(); err != nil {
					return nil, err
				}
			}
			continue
		}

		var hdr [2]byte
		if _, err := io.ReadFull(fr.r, hdr[:]); err != nil {
			return nil, err
		}
		length := int(binary.BigEndian.Uint16(hdr[:]))
		if length > packetMTU {
			// Corrupt/implausible length: this wasn't a real frame marker.
			continue
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(fr.r, body); err != nil {
			return nil, err
		}
		return body, nil
	}
}

// encodeFrame wraps a marshalled protobuf message for transmission:
// start1, start2, u16 big-endian length, message bytes.
func encodeFrame(data []byte) ([]byte, error) {
	if len(data) > packetMTU {
		return nil, io.ErrShortBuffer
	}
	out := make([]byte, 4+len(data))
	out[0] = start1
	out[1] = start2
	binary.BigEndian.PutUint16(out[2:4], uint16(len(data)))
	copy(out[4:], data)
	return out, nil
}

// wakeSequence is written once after connecting to wake a sleeping device,
// and again periodically thereafter by Session.keepAliveLoop as harmless
// keepalive padding: any correctly-implemented frameReader (ours included)
// safely skips these bytes while resyncing on the real start1/start2 marker.
func wakeSequence() []byte {
	buf := make([]byte, wakeByteCount)
	for i := range buf {
		buf[i] = start2
	}
	return buf
}
