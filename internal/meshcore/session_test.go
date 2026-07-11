package meshcore

import (
	"bytes"
	"encoding/binary"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRadio drives the server side of a net.Pipe, reading companion frames
// and handing them to onFrame, which returns the raw response frame bytes to
// write back (or nil to send nothing). Push additionally lets a test send an
// unsolicited frame (e.g. a 0x88 raw RF log) at any time.
type fakeRadio struct {
	conn    net.Conn
	fr      *FrameReader
	onFrame func(frame []byte) []byte

	writeMu sync.Mutex
}

func newFakeRadio(conn net.Conn, onFrame func(frame []byte) []byte) *fakeRadio {
	// The radio reads app -> radio frames, which are marked '<' (0x3C), the
	// opposite direction from the production FrameReader used by Session
	// (which reads radio -> app frames marked '>').
	fr := &fakeRadio{conn: conn, fr: newFrameReaderWithMarker(conn, frameMarkerToRadio), onFrame: onFrame}
	go fr.run()
	return fr
}

func (fr *fakeRadio) run() {
	for {
		frame, err := fr.fr.ReadFrame()
		if err != nil {
			return
		}
		if resp := fr.onFrame(frame); resp != nil {
			fr.Push(resp)
		}
	}
}

// Push writes an unsolicited (or response) frame to the client, safe to call
// concurrently with the run() loop's own responses. Radio -> app frames are
// marked '>', the opposite direction from EncodeFrame (app -> radio, '<').
func (fr *fakeRadio) Push(frame []byte) error {
	fr.writeMu.Lock()
	defer fr.writeMu.Unlock()
	_, err := fr.conn.Write(encodeFrameWithMarker(frame, frameMarkerFromRadio))
	return err
}

func selfInfoFrame(name string) []byte {
	out := make([]byte, 58+len(name))
	out[0] = FrameSelfInfo
	// pubkey/lat/lon/etc left zeroed; freq/bw/sf/cr are exercised separately.
	binary.LittleEndian.PutUint32(out[48:52], 869525) // 869.525 MHz
	binary.LittleEndian.PutUint32(out[52:56], 250000) // 250 kHz
	out[56] = 10                                      // SF
	out[57] = 5                                       // CR
	copy(out[58:], name)
	return out
}

func dialOverPipe(t *testing.T, onFrame func(frame []byte) []byte) (*Session, SelfInfo, *fakeRadio) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	radio := newFakeRadio(serverConn, onFrame)

	s, info, err := newSessionOverConn(clientConn, "hoplink-test")
	if err != nil {
		t.Fatalf("newSessionOverConn: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, info, radio
}

func TestSession_AppStartHandshake(t *testing.T) {
	_, info, _ := dialOverPipe(t, func(frame []byte) []byte {
		if frame[0] == CmdAppStart {
			return selfInfoFrame("MyNode")
		}
		return nil
	})
	if info.Name != "MyNode" {
		t.Errorf("info.Name = %q, want %q", info.Name, "MyNode")
	}
	if info.RadioSF != 10 || info.RadioCR != 5 {
		t.Errorf("SF/CR = %d/%d, want 10/5", info.RadioSF, info.RadioCR)
	}
}

func TestSession_SendRawPacket_OK(t *testing.T) {
	var gotFrame []byte
	s, _, _ := dialOverPipe(t, func(frame []byte) []byte {
		switch frame[0] {
		case CmdAppStart:
			return selfInfoFrame("Node")
		case CmdSendRawPacket:
			gotFrame = append([]byte(nil), frame...)
			return []byte{FrameOK}
		}
		return nil
	})

	packet := []byte{1, 2, 3}
	if err := s.SendRawPacket(packet, 7); err != nil {
		t.Fatalf("SendRawPacket: %v", err)
	}
	want := []byte{CmdSendRawPacket, 7, 1, 2, 3}
	if string(gotFrame) != string(want) {
		t.Errorf("radio received %v, want %v", gotFrame, want)
	}
}

func TestSession_SendChannelMessage_EncodesRequestedHashSize(t *testing.T) {
	var gotFrame []byte
	s, _, _ := dialOverPipe(t, func(frame []byte) []byte {
		switch frame[0] {
		case CmdAppStart:
			return selfInfoFrame("Node")
		case CmdSendRawPacket:
			gotFrame = append([]byte(nil), frame...)
			return []byte{FrameOK}
		}
		return nil
	})

	secret := HashtagChannelSecret("#test")
	if err := s.SendChannelMessage(secret, RouteFlood, 3, nil, "Alice: hi"); err != nil {
		t.Fatalf("SendChannelMessage: %v", err)
	}

	pkt, err := ParsePacket(gotFrame[2:]) // strip [cmd][priority]
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if pkt.HashSize != 3 {
		t.Errorf("HashSize = %d, want 3", pkt.HashSize)
	}
	if pkt.Route != RouteFlood {
		t.Errorf("Route = %v, want RouteFlood (unscoped, since scopeKey16 was nil)", pkt.Route)
	}
	if pkt.PayloadType != PayloadTypeGrpTxt {
		t.Errorf("PayloadType = %v, want GrpTxt", pkt.PayloadType)
	}
	dec, ok := DecryptGroupText(secret, pkt.Payload)
	if !ok || dec.Text != "Alice: hi" {
		t.Errorf("decrypted text = %q, ok=%v, want %q", dec.Text, ok, "Alice: hi")
	}
}

func TestSession_SendChannelMessage_AppliesFloodScope(t *testing.T) {
	var gotFrame []byte
	s, _, _ := dialOverPipe(t, func(frame []byte) []byte {
		switch frame[0] {
		case CmdAppStart:
			return selfInfoFrame("Node")
		case CmdSendRawPacket:
			gotFrame = append([]byte(nil), frame...)
			return []byte{FrameOK}
		}
		return nil
	})

	secret := HashtagChannelSecret("#test")
	scopeKey := FloodScopeKey("myregion")
	if err := s.SendChannelMessage(secret, RouteFlood, 3, scopeKey, "Alice: hi"); err != nil {
		t.Fatalf("SendChannelMessage: %v", err)
	}

	pkt, err := ParsePacket(gotFrame[2:])
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if pkt.Route != RouteTransportFlood {
		t.Errorf("Route = %v, want RouteTransportFlood (scoped)", pkt.Route)
	}
	if len(pkt.TransportCodes) != 4 {
		t.Fatalf("TransportCodes length = %d, want 4", len(pkt.TransportCodes))
	}
	dec, ok := DecryptGroupText(secret, pkt.Payload)
	if !ok || dec.Text != "Alice: hi" {
		t.Fatalf("decrypted text = %q, ok=%v, want %q", dec.Text, ok, "Alice: hi")
	}

	wantCode, err := CalcTransportCode(scopeKey, PayloadTypeGrpTxt, pkt.Payload)
	if err != nil {
		t.Fatalf("CalcTransportCode: %v", err)
	}
	gotCode := uint16(pkt.TransportCodes[0]) | uint16(pkt.TransportCodes[1])<<8
	if gotCode != wantCode {
		t.Errorf("transport_codes[0] = %#x, want %#x", gotCode, wantCode)
	}
	gotCode2 := uint16(pkt.TransportCodes[2]) | uint16(pkt.TransportCodes[3])<<8
	if gotCode2 != 0 {
		t.Errorf("transport_codes[1] = %#x, want 0", gotCode2)
	}
}

func TestSession_SendRawPacket_Err(t *testing.T) {
	s, _, _ := dialOverPipe(t, func(frame []byte) []byte {
		switch frame[0] {
		case CmdAppStart:
			return selfInfoFrame("Node")
		case CmdSendRawPacket:
			return []byte{FrameErr, ErrCodeTableFull}
		}
		return nil
	})

	err := s.SendRawPacket([]byte{1}, 0)
	if err == nil {
		t.Fatal("expected error")
	}
	errResp, ok := err.(*ErrResponse)
	if !ok {
		t.Fatalf("expected *ErrResponse, got %T: %v", err, err)
	}
	if errResp.Code != ErrCodeTableFull {
		t.Errorf("code = %d, want %d", errResp.Code, ErrCodeTableFull)
	}
}

func TestSession_SendRawPacket_RejectsOversizedWithoutWriting(t *testing.T) {
	wrote := false
	s, _, _ := dialOverPipe(t, func(frame []byte) []byte {
		if frame[0] == CmdAppStart {
			return selfInfoFrame("Node")
		}
		wrote = true
		return []byte{FrameOK}
	})

	huge := make([]byte, MaxRawPacketLen+1)
	if err := s.SendRawPacket(huge, 0); err == nil {
		t.Fatal("expected error for oversized packet")
	}
	if wrote {
		t.Fatal("should not have written an oversized packet frame")
	}
}

func TestSession_LogRxFrames_DeliversDecodedChannelMessages(t *testing.T) {
	secret := HashtagChannelSecret("#test")
	s, _, radio := dialOverPipe(t, func(frame []byte) []byte {
		if frame[0] == CmdAppStart {
			return selfInfoFrame("Node")
		}
		return nil
	})

	payload, err := BuildGroupTextPayload(secret, 1234, 0, "Alice: hi")
	if err != nil {
		t.Fatalf("BuildGroupTextPayload: %v", err)
	}
	packet, err := BuildPacket(Packet{Route: RouteFlood, PayloadType: PayloadTypeGrpTxt, Payload: payload})
	if err != nil {
		t.Fatalf("BuildPacket: %v", err)
	}
	var snr, rssi int8 = -20, -80
	rxFrame := append([]byte{PushLogRxData, byte(snr), byte(rssi)}, packet...)

	if err := radio.Push(rxFrame); err != nil {
		t.Fatalf("radio.Push: %v", err)
	}

	select {
	case lrx := <-s.LogRxFrames():
		if lrx.Packet.PayloadType != PayloadTypeGrpTxt {
			t.Fatalf("PayloadType = %v, want GrpTxt", lrx.Packet.PayloadType)
		}
		got, ok := DecryptGroupText(secret, lrx.Packet.Payload)
		if !ok {
			t.Fatalf("DecryptGroupText failed on delivered payload")
		}
		if got.Text != "Alice: hi" {
			t.Errorf("text = %q, want %q", got.Text, "Alice: hi")
		}
		if lrx.SNR != -5.0 {
			t.Errorf("SNR = %v, want -5.0", lrx.SNR)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for LogRxFrames delivery")
	}
}

func TestSession_PushFrames_DeliversRawUndecodedPush(t *testing.T) {
	s, _, radio := dialOverPipe(t, func(frame []byte) []byte {
		if frame[0] == CmdAppStart {
			return selfInfoFrame("Node")
		}
		return nil
	})

	// PushFrames must be called before the frame arrives: frames are only
	// forwarded (and drops only logged) once something has actually asked
	// for them (see Session.pushConsumed).
	frames := s.PushFrames()

	confirmFrame := []byte{PushAck, 1, 2, 3, 4}
	if err := radio.Push(confirmFrame); err != nil {
		t.Fatalf("radio.Push: %v", err)
	}

	select {
	case got := <-frames:
		if got[0] != PushAck {
			t.Errorf("push frame[0] = %#x, want %#x", got[0], PushAck)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for PushFrames delivery")
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent Write/String — needed
// because logf may be called from a Session's background goroutines
// (readLoop et al.) while the test's own goroutine reads the captured
// output, which a plain bytes.Buffer doesn't support.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog redirects the standard logger's output to a buffer for the
// duration of the test, restoring it on cleanup.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	orig := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return buf
}

func TestSession_PushFrames_NoSpamBeforeEverConsumed(t *testing.T) {
	_, _, radio := dialOverPipe(t, func(frame []byte) []byte {
		if frame[0] == CmdAppStart {
			return selfInfoFrame("Node")
		}
		return nil
	})
	buf := captureLog(t)

	// Nobody has ever called PushFrames: pushing far more than pushCh's
	// buffer size must not log any "dropped push frame" spam, since a
	// channel nobody has asked for isn't meaningfully "full".
	for i := 0; i < 40; i++ {
		if err := radio.Push([]byte{PushAck, byte(i)}); err != nil {
			t.Fatalf("radio.Push: %v", err)
		}
	}
	time.Sleep(100 * time.Millisecond)

	if strings.Contains(buf.String(), "dropped push frame") {
		t.Errorf("expected no drop-spam before PushFrames was ever called, got: %s", buf.String())
	}
}

func TestSession_PushFrames_LogsDropsOnceConsumed(t *testing.T) {
	s, _, radio := dialOverPipe(t, func(frame []byte) []byte {
		if frame[0] == CmdAppStart {
			return selfInfoFrame("Node")
		}
		return nil
	})
	_ = s.PushFrames() // register interest, but never drain it below
	buf := captureLog(t)

	// Now that something has asked for pushCh, filling past its buffer
	// should log drops as before.
	for i := 0; i < 40; i++ {
		if err := radio.Push([]byte{PushAck, byte(i)}); err != nil {
			t.Fatalf("radio.Push: %v", err)
		}
	}
	time.Sleep(100 * time.Millisecond)

	if !strings.Contains(buf.String(), "dropped push frame") {
		t.Errorf("expected drop logging once PushFrames had been consumed, got: %s", buf.String())
	}
}

func TestSession_Close_UnblocksPendingRequest(t *testing.T) {
	s, _, _ := dialOverPipe(t, func(frame []byte) []byte {
		if frame[0] == CmdAppStart {
			return selfInfoFrame("Node")
		}
		return nil // never respond to SendRawPacket
	})

	done := make(chan error, 1)
	go func() {
		done <- s.SendRawPacket([]byte{1}, 0)
	}()

	time.Sleep(10 * time.Millisecond)
	_ = s.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error after Close during a pending request")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock the pending request")
	}
}

// TestSession_ConcurrentRequests_AreSerialisedNotCorrupted exercises cmdMu:
// two goroutines calling request() (via SendRawPacket) at the same time —
// exactly what happens once the keepalive loop and a real bridge send can
// overlap — must each complete without hanging or receiving a corrupted
// response, and the fake radio must see both distinct frames intact.
// TestSession_SendRawPacket_IgnoresStrayFrameBeforeRealResponse reproduces
// a real bug: the radio can emit an unrelated frame (e.g. a delayed or
// unsolicited RESP_CODE_CURR_TIME, 0x09 — this exact scenario broke a live
// CMD_SEND_RAW_PACKET send) while we're waiting for a command's OK/ERR. The
// stray frame must be discarded, not misattributed as the answer.
func TestSession_SendRawPacket_IgnoresStrayFrameBeforeRealResponse(t *testing.T) {
	var radio *fakeRadio
	s, _, r := dialOverPipe(t, func(frame []byte) []byte {
		switch frame[0] {
		case CmdAppStart:
			return selfInfoFrame("Node")
		case CmdSendRawPacket:
			go func() {
				// Simulate an unrelated frame arriving first (e.g. a
				// delayed reply to some earlier, already-abandoned
				// command), then the real OK a moment later.
				_ = radio.Push([]byte{FrameCurrTime, 1, 2, 3, 4})
				time.Sleep(20 * time.Millisecond)
				_ = radio.Push([]byte{FrameOK})
			}()
			return nil // handled by the goroutine above instead
		}
		return nil
	})
	radio = r

	if err := s.SendRawPacket([]byte{1, 2, 3}, 0); err != nil {
		t.Fatalf("SendRawPacket: %v (stray frame should have been discarded, not misattributed)", err)
	}
}

func TestSession_ConcurrentRequests_AreSerialisedNotCorrupted(t *testing.T) {
	var mu sync.Mutex
	var gotFrames [][]byte
	s, _, _ := dialOverPipe(t, func(frame []byte) []byte {
		if frame[0] == CmdAppStart {
			return selfInfoFrame("Node")
		}
		if frame[0] == CmdSendRawPacket {
			mu.Lock()
			gotFrames = append(gotFrames, append([]byte(nil), frame...))
			mu.Unlock()
			// A brief delay increases the odds of exposing any missing
			// mutual exclusion as flaky cross-talk rather than luck.
			time.Sleep(5 * time.Millisecond)
			return []byte{FrameOK}
		}
		return nil
	})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = s.SendRawPacket([]byte{0xAA}, 1)
	}()
	go func() {
		defer wg.Done()
		errs[1] = s.SendRawPacket([]byte{0xBB}, 2)
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotFrames) != 2 {
		t.Fatalf("radio received %d CMD_SEND_RAW_PACKET frames, want 2", len(gotFrames))
	}
	want := map[string]bool{
		string([]byte{CmdSendRawPacket, 1, 0xAA}): false,
		string([]byte{CmdSendRawPacket, 2, 0xBB}): false,
	}
	for _, f := range gotFrames {
		if _, ok := want[string(f)]; !ok {
			t.Errorf("unexpected/corrupted frame: %v", f)
		}
		want[string(f)] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("expected frame not received: %q", k)
		}
	}
}
