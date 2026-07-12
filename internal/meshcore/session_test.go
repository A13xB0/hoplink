package meshcore

import (
	"bytes"
	"encoding/binary"
	"fmt"
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
	if err := s.SendChannelMessage(secret, 3, nil, "Alice: hi"); err != nil {
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
	if err := s.SendChannelMessage(secret, 3, scopeKey, "Alice: hi"); err != nil {
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

// fakeChannelDevice simulates a device's channel table (CMD_GET_CHANNEL/
// CMD_SET_CHANNEL, indices 1-MaxChannelSlots) and its queued-message store
// (CMD_SYNC_NEXT_MESSAGE), for RegisterChannel/syncLoop tests. Absent slots
// read back as empty (name "", secret all-zero), matching a never-configured
// device slot.
type fakeChannelDevice struct {
	mu       sync.Mutex
	slots    map[byte]ChannelInfo
	setCalls []ChannelInfo // every CMD_SET_CHANNEL received, in order
	queue    [][]byte      // pre-built response frames, returned in order by CMD_SYNC_NEXT_MESSAGE
}

func (d *fakeChannelDevice) handle(frame []byte) []byte {
	if len(frame) == 0 {
		return nil
	}
	switch frame[0] {
	case CmdAppStart:
		return selfInfoFrame("Node")
	case CmdGetChannel:
		idx := frame[1]
		d.mu.Lock()
		info, ok := d.slots[idx]
		d.mu.Unlock()
		if !ok {
			info = ChannelInfo{Index: idx, Name: "", Secret: make([]byte, 16)}
		}
		resp := make([]byte, 2+channelNameFieldLen+16)
		resp[0] = FrameChannelInfo
		resp[1] = idx
		copy(resp[2:2+channelNameFieldLen], info.Name)
		copy(resp[2+channelNameFieldLen:], info.Secret)
		return resp
	case CmdSetChannel:
		info, ok := ParseChannelInfo(append([]byte{FrameChannelInfo}, frame[1:]...))
		if !ok {
			return []byte{FrameErr, ErrCodeIllegalArg}
		}
		d.mu.Lock()
		if d.slots == nil {
			d.slots = map[byte]ChannelInfo{}
		}
		d.slots[info.Index] = info
		d.setCalls = append(d.setCalls, info)
		d.mu.Unlock()
		return []byte{FrameOK}
	case CmdSyncNextMessage:
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(d.queue) == 0 {
			return []byte{FrameNoMoreMessages}
		}
		next := d.queue[0]
		d.queue = d.queue[1:]
		return next
	default:
		return nil
	}
}

func buildChannelMsgRecvFrame(idx byte, timestampUnix uint32, text string) []byte {
	f := []byte{FrameChannelMsgRecv, idx, 0xFF, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(f[4:8], timestampUnix)
	return append(f, []byte(text)...)
}

func TestSession_RegisterChannel_ClaimsFirstEmptySlot(t *testing.T) {
	dev := &fakeChannelDevice{}
	s, _, _ := dialOverPipe(t, dev.handle)

	secret := HashtagChannelSecret("#general")
	idx, alreadyInstalled, err := s.RegisterChannel(secret, "general")
	if err != nil {
		t.Fatalf("RegisterChannel: %v", err)
	}
	if idx != 1 {
		t.Errorf("idx = %d, want 1 (first empty slot)", idx)
	}
	if alreadyInstalled {
		t.Error("alreadyInstalled = true, want false (freshly registered)")
	}
	if len(dev.setCalls) != 1 {
		t.Fatalf("expected exactly 1 CMD_SET_CHANNEL, got %d", len(dev.setCalls))
	}
	if dev.setCalls[0].Name != "general" || !bytes.Equal(dev.setCalls[0].Secret, secret) {
		t.Errorf("SET_CHANNEL = %+v, want name=general secret=%x", dev.setCalls[0], secret)
	}
}

func TestSession_RegisterChannel_ReusesExistingSlot(t *testing.T) {
	secret := HashtagChannelSecret("#general")
	dev := &fakeChannelDevice{slots: map[byte]ChannelInfo{
		3: {Index: 3, Name: "general", Secret: secret},
	}}
	s, _, _ := dialOverPipe(t, dev.handle)

	idx, alreadyInstalled, err := s.RegisterChannel(secret, "general")
	if err != nil {
		t.Fatalf("RegisterChannel: %v", err)
	}
	if idx != 3 {
		t.Errorf("idx = %d, want 3 (existing slot, reused)", idx)
	}
	if !alreadyInstalled {
		t.Error("alreadyInstalled = false, want true (reused an existing slot)")
	}
	if len(dev.setCalls) != 0 {
		t.Errorf("expected no CMD_SET_CHANNEL when the secret is already registered, got %d", len(dev.setCalls))
	}
}

func TestSession_RegisterChannel_DoesNotClobberDifferentSecretInSlot1(t *testing.T) {
	other := HashtagChannelSecret("#other")
	dev := &fakeChannelDevice{slots: map[byte]ChannelInfo{
		1: {Index: 1, Name: "other", Secret: other},
	}}
	s, _, _ := dialOverPipe(t, dev.handle)

	secret := HashtagChannelSecret("#general")
	idx, _, err := s.RegisterChannel(secret, "general")
	if err != nil {
		t.Fatalf("RegisterChannel: %v", err)
	}
	if idx == 1 {
		t.Fatal("must not overwrite slot 1, which holds an unrelated channel")
	}
	if len(dev.setCalls) != 1 || dev.setCalls[0].Index == 1 {
		t.Errorf("expected registration to target a different slot, got %+v", dev.setCalls)
	}
}

func TestSession_RegisterChannel_ErrorsWhenAllSlotsFull(t *testing.T) {
	slots := map[byte]ChannelInfo{}
	for i := byte(1); i <= MaxChannelSlots; i++ {
		slots[i] = ChannelInfo{Index: i, Name: fmt.Sprintf("chan%d", i), Secret: HashtagChannelSecret(fmt.Sprintf("#chan%d", i))}
	}
	dev := &fakeChannelDevice{slots: slots}
	s, _, _ := dialOverPipe(t, dev.handle)

	if _, _, err := s.RegisterChannel(HashtagChannelSecret("#new"), "new"); err == nil {
		t.Fatal("expected an error when all slots are occupied by unrelated channels")
	}
}

func TestSession_RegisterPublicChannel_InstallsAtSlotZeroWhenEmpty(t *testing.T) {
	dev := &fakeChannelDevice{}
	s, _, _ := dialOverPipe(t, dev.handle)

	alreadyInstalled, err := s.RegisterPublicChannel()
	if err != nil {
		t.Fatalf("RegisterPublicChannel: %v", err)
	}
	if alreadyInstalled {
		t.Error("alreadyInstalled = true, want false (freshly registered)")
	}
	if len(dev.setCalls) != 1 {
		t.Fatalf("expected exactly 1 CMD_SET_CHANNEL, got %d", len(dev.setCalls))
	}
	if dev.setCalls[0].Index != PublicChannelSlot {
		t.Errorf("SET_CHANNEL index = %d, want %d", dev.setCalls[0].Index, PublicChannelSlot)
	}
	if !bytes.Equal(dev.setCalls[0].Secret, PublicChannelKey) {
		t.Errorf("SET_CHANNEL secret = %x, want the well-known public key %x", dev.setCalls[0].Secret, PublicChannelKey)
	}
}

func TestSession_RegisterPublicChannel_ReusesExistingSlotZero(t *testing.T) {
	dev := &fakeChannelDevice{slots: map[byte]ChannelInfo{
		PublicChannelSlot: {Index: PublicChannelSlot, Name: "public", Secret: PublicChannelKey},
	}}
	s, _, _ := dialOverPipe(t, dev.handle)

	alreadyInstalled, err := s.RegisterPublicChannel()
	if err != nil {
		t.Fatalf("RegisterPublicChannel: %v", err)
	}
	if !alreadyInstalled {
		t.Error("alreadyInstalled = false, want true (already at slot 0)")
	}
	if len(dev.setCalls) != 0 {
		t.Errorf("expected no CMD_SET_CHANNEL when the public channel is already installed, got %d", len(dev.setCalls))
	}
}

func TestSession_RegisterPublicChannel_ErrorsWhenSlotZeroHoldsSomethingElse(t *testing.T) {
	dev := &fakeChannelDevice{slots: map[byte]ChannelInfo{
		PublicChannelSlot: {Index: PublicChannelSlot, Name: "not-public", Secret: HashtagChannelSecret("#not-public")},
	}}
	s, _, _ := dialOverPipe(t, dev.handle)

	if _, err := s.RegisterPublicChannel(); err == nil {
		t.Fatal("expected an error rather than overwriting an unrelated channel at slot 0")
	}
	if len(dev.setCalls) != 0 {
		t.Errorf("expected no CMD_SET_CHANNEL when refusing to overwrite slot 0, got %d", len(dev.setCalls))
	}
}

func TestSession_SyncLoop_DrainsOnPushMsgWaiting(t *testing.T) {
	dev := &fakeChannelDevice{queue: [][]byte{
		buildChannelMsgRecvFrame(2, 1700000000, "Alice: hi"),
	}}
	s, _, radio := dialOverPipe(t, dev.handle)

	if err := radio.Push([]byte{PushMsgWaiting}); err != nil {
		t.Fatalf("radio.Push: %v", err)
	}

	select {
	case msg := <-s.ChannelMessages():
		if msg.SlotIndex != 2 || msg.Text != "Alice: hi" || msg.TimestampUnix != 1700000000 {
			t.Errorf("got %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for synced channel message")
	}
}

func TestSession_SyncLoop_DrainsMultipleMessagesInOneCycle(t *testing.T) {
	dev := &fakeChannelDevice{queue: [][]byte{
		buildChannelMsgRecvFrame(1, 1700000000, "Alice: one"),
		buildChannelMsgRecvFrame(1, 1700000001, "Bob: two"),
		buildChannelMsgRecvFrame(1, 1700000002, "Carol: three"),
	}}
	s, _, radio := dialOverPipe(t, dev.handle)

	if err := radio.Push([]byte{PushMsgWaiting}); err != nil {
		t.Fatalf("radio.Push: %v", err)
	}

	var got []string
	for i := 0; i < 3; i++ {
		select {
		case msg := <-s.ChannelMessages():
			got = append(got, msg.Text)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for message %d", i)
		}
	}
	want := []string{"Alice: one", "Bob: two", "Carol: three"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSession_SyncLoop_PeriodicPollDrainsWithoutPush(t *testing.T) {
	orig := syncPollInterval
	syncPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { syncPollInterval = orig })

	dev := &fakeChannelDevice{queue: [][]byte{
		buildChannelMsgRecvFrame(1, 1700000000, "Alice: polled"),
	}}
	s, _, _ := dialOverPipe(t, dev.handle) // no PUSH_CODE_MSG_WAITING sent at all

	select {
	case msg := <-s.ChannelMessages():
		if msg.Text != "Alice: polled" {
			t.Errorf("Text = %q, want %q", msg.Text, "Alice: polled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the periodic poll to drain the queued message")
	}
}

func TestSession_SyncLoop_IgnoresContactAndDataMessages(t *testing.T) {
	// A contact (direct) message and a channel-data datagram in the queue,
	// followed by a real channel message — must skip the first two without
	// blocking delivery of the one we do support.
	contactFrame := []byte{FrameContactMsgRecv, 1, 2, 3, 4, 5, 6, 0xFF, 0, 0, 0, 0, 0}
	dataFrame := []byte{FrameChannelDataRecv, 0, 0, 0, 1, 0xFF, 0xFF, 0xFF, 0}
	dev := &fakeChannelDevice{queue: [][]byte{
		contactFrame,
		dataFrame,
		buildChannelMsgRecvFrame(1, 1700000000, "Alice: real one"),
	}}
	s, _, radio := dialOverPipe(t, dev.handle)

	if err := radio.Push([]byte{PushMsgWaiting}); err != nil {
		t.Fatalf("radio.Push: %v", err)
	}

	select {
	case msg := <-s.ChannelMessages():
		if msg.Text != "Alice: real one" {
			t.Errorf("Text = %q, want %q", msg.Text, "Alice: real one")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the real channel message past the ignored ones")
	}
}
