package meshtastic

import (
	"io"
	"net"
	"testing"
	"time"

	generated "github.com/meshnet-gophers/meshtastic-go/meshtastic"
	"google.golang.org/protobuf/proto"
)

// fakeRadio drives the far end of a real TCP connection, decoding ToRadio
// frames the client sends and replying with FromRadio frames. Meshtastic's
// stream framing is symmetric (both directions use the same 0x94/0xC3 +
// big-endian-length header), so the package's own frameReader/encodeFrame
// work for both sides without needing a MeshCore-style direction marker.
type fakeRadio struct {
	t    *testing.T
	conn net.Conn
	fr   *frameReader

	onToRadio func(msg *generated.ToRadio) []*generated.FromRadio
}

func newFakeRadio(t *testing.T, conn net.Conn, onToRadio func(*generated.ToRadio) []*generated.FromRadio) *fakeRadio {
	t.Helper()
	fr := &fakeRadio{t: t, conn: conn, fr: newFrameReader(conn), onToRadio: onToRadio}
	go fr.run()
	return fr
}

func (fr *fakeRadio) run() {
	for {
		frame, err := fr.fr.readFrame()
		if err != nil {
			return
		}
		var toRadio generated.ToRadio
		if err := proto.Unmarshal(frame, &toRadio); err != nil {
			continue
		}
		if fr.onToRadio == nil {
			continue
		}
		for _, reply := range fr.onToRadio(&toRadio) {
			fr.push(reply)
		}
	}
}

// push writes an unsolicited (or reply) FromRadio message to the client.
func (fr *fakeRadio) push(msg *generated.FromRadio) {
	data, err := proto.Marshal(msg)
	if err != nil {
		fr.t.Errorf("marshalling FromRadio: %v", err)
		return
	}
	frame, err := encodeFrame(data)
	if err != nil {
		fr.t.Errorf("encodeFrame: %v", err)
		return
	}
	_, _ = fr.conn.Write(frame)
}

// startFakeRadio starts a TCP listener and, once a connection arrives,
// drives it with a fakeRadio using onToRadio. Returns the address to dial
// and a channel that receives the fakeRadio once the connection lands (so
// tests can push unsolicited messages after the handshake completes).
func startFakeRadio(t *testing.T, onToRadio func(*generated.ToRadio) []*generated.FromRadio) (addr string, radioCh chan *fakeRadio) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	radioCh = make(chan *fakeRadio, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		radioCh <- newFakeRadio(t, conn, onToRadio)
	}()
	return ln.Addr().String(), radioCh
}

// standardHandshakeReplies builds the FromRadio sequence a real device
// sends in response to want_config: my_info, a couple of node_info
// entries, two channel slots (primary with a blank name, and "general" at
// index 1), then config_complete_id echoing nonce.
func standardHandshakeReplies(nonce uint32) []*generated.FromRadio {
	return []*generated.FromRadio{
		{PayloadVariant: &generated.FromRadio_MyInfo{MyInfo: &generated.MyNodeInfo{MyNodeNum: 42}}},
		{PayloadVariant: &generated.FromRadio_NodeInfo{NodeInfo: &generated.NodeInfo{
			Num:  99,
			User: &generated.User{LongName: "Alice Node", ShortName: "ALI"},
		}}},
		{PayloadVariant: &generated.FromRadio_Channel{Channel: &generated.Channel{
			Index:    0,
			Role:     generated.Channel_PRIMARY,
			Settings: &generated.ChannelSettings{Name: ""},
		}}},
		{PayloadVariant: &generated.FromRadio_Channel{Channel: &generated.Channel{
			Index:    1,
			Role:     generated.Channel_SECONDARY,
			Settings: &generated.ChannelSettings{Name: "general"},
		}}},
		{PayloadVariant: &generated.FromRadio_ConfigCompleteId{ConfigCompleteId: nonce}},
	}
}

func dialAgainstStandardHandshake(t *testing.T, extraOnToRadio func(*generated.ToRadio)) (*Session, chan *fakeRadio) {
	t.Helper()
	addr, radioCh := startFakeRadio(t, func(msg *generated.ToRadio) []*generated.FromRadio {
		if w, ok := msg.PayloadVariant.(*generated.ToRadio_WantConfigId); ok {
			return standardHandshakeReplies(w.WantConfigId)
		}
		if extraOnToRadio != nil {
			extraOnToRadio(msg)
		}
		return nil
	})
	s, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, radioCh
}

func TestDial_PerformsHandshakeAndCollectsChannelsAndNodes(t *testing.T) {
	s, _ := dialAgainstStandardHandshake(t, nil)

	if s.MyNodeNum() != 42 {
		t.Errorf("MyNodeNum() = %d, want 42", s.MyNodeNum())
	}
	if idx, ok := s.ResolveChannelIndex("general"); !ok || idx != 1 {
		t.Errorf("ResolveChannelIndex(general) = (%d, %v), want (1, true)", idx, ok)
	}
	if idx, ok := s.ResolveChannelIndex("LongFast"); !ok || idx != 0 {
		t.Errorf("ResolveChannelIndex(LongFast) = (%d, %v), want (0, true) via primary-name fallback", idx, ok)
	}
	if idx, ok := s.ResolveChannelIndex("GENERAL"); !ok || idx != 1 {
		t.Errorf("channel name matching should be case-insensitive, got (%d, %v)", idx, ok)
	}
	if _, ok := s.ResolveChannelIndex("nonexistent"); ok {
		t.Error("expected ResolveChannelIndex to fail for a channel the device doesn't have configured")
	}
}

// TestDial_DisabledBlankNamedSlotsDoNotClobberThePrimaryChannel reproduces a
// real bug: a device sends a Channel dump for every slot (0-7), and unused
// slots (Role Channel_DISABLED, the zero value) commonly also have a blank
// name — same as the primary channel's own conventionally-blank name. If
// disabled slots aren't excluded, whichever blank-named slot is processed
// last wins the name->index entry, silently pointing "primary channel"
// lookups at a disabled slot instead of the real primary (index 0).
func TestDial_DisabledBlankNamedSlotsDoNotClobberThePrimaryChannel(t *testing.T) {
	addr, _ := startFakeRadio(t, func(msg *generated.ToRadio) []*generated.FromRadio {
		w, ok := msg.PayloadVariant.(*generated.ToRadio_WantConfigId)
		if !ok {
			return nil
		}
		replies := []*generated.FromRadio{
			{PayloadVariant: &generated.FromRadio_MyInfo{MyInfo: &generated.MyNodeInfo{MyNodeNum: 1}}},
			{PayloadVariant: &generated.FromRadio_Channel{Channel: &generated.Channel{
				Index: 0, Role: generated.Channel_PRIMARY, Settings: &generated.ChannelSettings{Name: ""},
			}}},
		}
		// Slots 1-7: disabled, blank name — exactly what a stock device
		// sends for its unused secondary channel slots.
		for i := 1; i <= 7; i++ {
			replies = append(replies, &generated.FromRadio{PayloadVariant: &generated.FromRadio_Channel{Channel: &generated.Channel{
				Index: int32(i), Role: generated.Channel_DISABLED, Settings: &generated.ChannelSettings{Name: ""},
			}}})
		}
		replies = append(replies, &generated.FromRadio{PayloadVariant: &generated.FromRadio_ConfigCompleteId{ConfigCompleteId: w.WantConfigId}})
		return replies
	})

	s, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	idx, ok := s.ResolveChannelIndex("LongFast")
	if !ok || idx != 0 {
		t.Errorf("ResolveChannelIndex(LongFast) = (%d, %v), want (0, true) — a disabled slot clobbered the primary channel's entry", idx, ok)
	}
}

func TestSendText_UsesResolvedChannelIndexAndBroadcasts(t *testing.T) {
	sentPackets := make(chan *generated.MeshPacket, 4)
	addr, _ := startFakeRadio(t, func(msg *generated.ToRadio) []*generated.FromRadio {
		if w, ok := msg.PayloadVariant.(*generated.ToRadio_WantConfigId); ok {
			return standardHandshakeReplies(w.WantConfigId)
		}
		if p, ok := msg.PayloadVariant.(*generated.ToRadio_Packet); ok {
			sentPackets <- p.Packet
		}
		return nil
	})
	s, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.SendText("general", "Alice: hello mesh", 7); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	select {
	case pkt := <-sentPackets:
		if pkt.Channel != 1 {
			t.Errorf("Channel = %d, want 1", pkt.Channel)
		}
		if pkt.To != BroadcastAddr {
			t.Errorf("To = %#x, want BroadcastAddr", pkt.To)
		}
		if pkt.HopLimit != 7 {
			t.Errorf("HopLimit = %d, want 7", pkt.HopLimit)
		}
		decoded, ok := pkt.PayloadVariant.(*generated.MeshPacket_Decoded)
		if !ok {
			t.Fatalf("PayloadVariant = %T, want MeshPacket_Decoded", pkt.PayloadVariant)
		}
		if decoded.Decoded.Portnum != generated.PortNum_TEXT_MESSAGE_APP {
			t.Errorf("Portnum = %v, want TEXT_MESSAGE_APP", decoded.Decoded.Portnum)
		}
		if string(decoded.Decoded.Payload) != "Alice: hello mesh" {
			t.Errorf("Payload = %q, want %q", decoded.Decoded.Payload, "Alice: hello mesh")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the fake radio to receive a packet")
	}
}

func TestSendText_UsesConfiguredHopLimit(t *testing.T) {
	sentPackets := make(chan *generated.MeshPacket, 4)
	addr, _ := startFakeRadio(t, func(msg *generated.ToRadio) []*generated.FromRadio {
		if w, ok := msg.PayloadVariant.(*generated.ToRadio_WantConfigId); ok {
			return standardHandshakeReplies(w.WantConfigId)
		}
		if p, ok := msg.PayloadVariant.(*generated.ToRadio_Packet); ok {
			sentPackets <- p.Packet
		}
		return nil
	})
	s, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.SendText("general", "hi", 0); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	select {
	case pkt := <-sentPackets:
		if pkt.HopLimit != 0 {
			t.Errorf("HopLimit = %d, want 0 (explicit no-rebroadcast)", pkt.HopLimit)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the fake radio to receive a packet")
	}
}

func TestSendText_UnknownChannelReturnsError(t *testing.T) {
	s, _ := dialAgainstStandardHandshake(t, nil)
	if err := s.SendText("does-not-exist", "hi", 7); err == nil {
		t.Fatal("expected an error for an unconfigured channel name")
	}
}

func TestTextMessages_DeliversWithResolvedNodeName(t *testing.T) {
	s, radioCh := dialAgainstStandardHandshake(t, nil)
	radio := <-radioCh

	pkt := &generated.MeshPacket{
		From:    99, // Alice Node, from standardHandshakeReplies' node_info dump
		Channel: 1,
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_TEXT_MESSAGE_APP,
			Payload: []byte("hello from the mesh"),
		}},
	}
	radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: pkt}})

	select {
	case msg := <-s.TextMessages():
		if msg.From != 99 || msg.FromName != "Alice Node" || msg.ChannelIndex != 1 || msg.Text != "hello from the mesh" {
			t.Errorf("got %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TextMessages delivery")
	}
}

func TestTextMessages_UnknownNodeFallsBackToHexID(t *testing.T) {
	s, radioCh := dialAgainstStandardHandshake(t, nil)
	radio := <-radioCh

	pkt := &generated.MeshPacket{
		From:    0xDEADBEEF,
		Channel: 0,
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_TEXT_MESSAGE_APP,
			Payload: []byte("hi"),
		}},
	}
	radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: pkt}})

	select {
	case msg := <-s.TextMessages():
		if msg.FromName != "!deadbeef" {
			t.Errorf("FromName = %q, want %q", msg.FromName, "!deadbeef")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TextMessages delivery")
	}
}

func TestTextMessages_IgnoresEncryptedUndecodablePackets(t *testing.T) {
	s, radioCh := dialAgainstStandardHandshake(t, nil)
	radio := <-radioCh

	radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From:           1,
		PayloadVariant: &generated.MeshPacket_Encrypted{Encrypted: []byte{1, 2, 3}},
	}}})

	select {
	case msg := <-s.TextMessages():
		t.Fatalf("unexpected message from an undecodable encrypted packet: %+v", msg)
	case <-time.After(200 * time.Millisecond):
		// expected: nothing delivered
	}
}

func TestHandlePacket_LiveNodeInfoBroadcastUpdatesNodeDB(t *testing.T) {
	s, radioCh := dialAgainstStandardHandshake(t, nil)
	radio := <-radioCh

	userBytes, err := proto.Marshal(&generated.User{LongName: "Bob Later"})
	if err != nil {
		t.Fatalf("marshal User: %v", err)
	}
	radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From: 55,
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_NODEINFO_APP,
			Payload: userBytes,
		}},
	}}})

	// Give the read loop a moment to process the NODEINFO_APP update before
	// the text message that depends on it.
	time.Sleep(50 * time.Millisecond)

	radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From:    55,
		Channel: 0,
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_TEXT_MESSAGE_APP,
			Payload: []byte("hi"),
		}},
	}}})

	select {
	case msg := <-s.TextMessages():
		if msg.FromName != "Bob Later" {
			t.Errorf("FromName = %q, want %q", msg.FromName, "Bob Later")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TextMessages delivery")
	}
}

func TestDial_FailsPromptlyIfConnectionDropsMidHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Simulate the device dropping the connection mid-handshake
		// (before ever sending config_complete_id) — Dial should fail
		// promptly via a read error, not by waiting out the full handshake
		// timeout.
		_ = conn.Close()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := Dial(ln.Addr().String())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when the connection drops mid-handshake")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dial did not notice the dropped connection promptly")
	}
}

func TestSession_KeepAliveLoop_WritesPeriodicWakeBytes(t *testing.T) {
	orig := keepAliveInterval
	keepAliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = orig })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	connCh := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Perform the handshake by hand (rather than via fakeRadio, whose
		// own readFrame loop would otherwise compete with this test for
		// bytes off the same conn afterwards).
		fr := newFrameReader(conn)
		frame, err := fr.readFrame()
		if err != nil {
			return
		}
		var toRadio generated.ToRadio
		if err := proto.Unmarshal(frame, &toRadio); err != nil {
			return
		}
		w, ok := toRadio.PayloadVariant.(*generated.ToRadio_WantConfigId)
		if !ok {
			return
		}
		for _, reply := range standardHandshakeReplies(w.WantConfigId) {
			data, err := proto.Marshal(reply)
			if err != nil {
				return
			}
			f, err := encodeFrame(data)
			if err != nil {
				return
			}
			if _, err := conn.Write(f); err != nil {
				return
			}
		}
		connCh <- conn
	}()

	s, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var conn net.Conn
	select {
	case conn = <-connCh:
	case <-time.After(2 * time.Second):
		t.Fatal("handshake did not complete")
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, wakeByteCount)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("expected a keepalive wake sequence to arrive, got: %v", err)
	}
	for i, b := range buf {
		if b != start2 {
			t.Fatalf("keepalive byte[%d] = %#x, want %#x (start2)", i, b, start2)
		}
	}
}
