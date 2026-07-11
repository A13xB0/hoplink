package bridge

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
)

// tinyFakeRadio is a minimal companion-protocol responder over a real TCP
// listener (meshcore.Dial only accepts "host:port", not an arbitrary
// net.Conn, so unlike the meshcore package's own net.Pipe-based tests, the
// bridge package drives a real loopback listener). It only needs to satisfy
// APP_START with a SELF_INFO reply and, optionally, react to further
// frames via onFrame.
type tinyFakeRadio struct {
	t       *testing.T
	ln      net.Listener
	conn    net.Conn
	onFrame func(cmd byte, frame []byte) []byte // returns response frame, or nil for none
}

// startTinyFakeRadio starts listening and returns the address to dial plus
// the fake radio; the connection itself is accepted lazily on first Dial.
func startTinyFakeRadio(t *testing.T, onFrame func(cmd byte, frame []byte) []byte) (addr string, radio *tinyFakeRadio) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	r := &tinyFakeRadio{t: t, ln: ln, onFrame: onFrame}
	go r.acceptAndServe()
	return ln.Addr().String(), r
}

func (r *tinyFakeRadio) acceptAndServe() {
	conn, err := r.ln.Accept()
	if err != nil {
		return
	}
	r.conn = conn
	for {
		frame, err := readAppToRadioFrame(conn)
		if err != nil {
			return
		}
		if len(frame) == 0 {
			continue
		}
		if resp := r.onFrame(frame[0], frame); resp != nil {
			if err := writeRadioToAppFrame(conn, resp); err != nil {
				return
			}
		}
	}
}

// Push sends an unsolicited radio -> app frame (e.g. a 0x88 raw RF log) once
// the connection has been accepted.
func (r *tinyFakeRadio) Push(frame []byte) error {
	return writeRadioToAppFrame(r.conn, frame)
}

// readAppToRadioFrame reads one '<' (0x3C) + u16-LE length + body frame.
func readAppToRadioFrame(r io.Reader) ([]byte, error) {
	var b [1]byte
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		if b[0] == 0x3C {
			break
		}
	}
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint16(hdr[:])
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// writeRadioToAppFrame writes one '>' (0x3E) + u16-LE length + body frame.
func writeRadioToAppFrame(w io.Writer, frame []byte) error {
	out := make([]byte, 3+len(frame))
	out[0] = 0x3E
	binary.LittleEndian.PutUint16(out[1:3], uint16(len(frame)))
	copy(out[3:], frame)
	_, err := w.Write(out)
	return err
}

func selfInfoFrame(name string) []byte {
	out := make([]byte, 58+len(name))
	out[0] = 0x05 // FrameSelfInfo
	copy(out[58:], name)
	return out
}
