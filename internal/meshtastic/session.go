package meshtastic

import (
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"

	generated "github.com/meshnet-gophers/meshtastic-go/meshtastic"
	"google.golang.org/protobuf/proto"
)

func logf(format string, args ...any) {
	log.Printf("[meshtastic] "+format, args...)
}

// BroadcastAddr is the well-known Meshtastic "send to everyone on this
// channel" destination node number.
const BroadcastAddr uint32 = 0xFFFFFFFF

// DefaultCommandTimeout bounds how long Dial waits for the want_config
// handshake to complete (my_info/node_info/channel dump + config_complete_id).
const DefaultCommandTimeout = 10 * time.Second

// wellKnownPrimaryNames are names that should resolve to the device's
// primary channel (index 0) even if that slot's own stored Settings.Name is
// empty — which it commonly is, since primary channels are usually
// identified to humans by their LoRa modem preset name (e.g. "LongFast")
// rather than a name stored in the channel itself.
var wellKnownPrimaryNames = map[string]bool{
	"": true, "primary": true, "default": true,
	"longfast": true, "longslow": true, "vlongslow": true,
	"mediumslow": true, "mediumfast": true,
	"shortslow": true, "shortfast": true, "shortturbo": true,
}

// TextMessage is a decoded TEXT_MESSAGE_APP packet heard on the mesh.
type TextMessage struct {
	From         uint32
	FromName     string // resolved via the node DB; falls back to "!<hex node id>"
	ChannelIndex uint32
	PacketID     uint32 // MeshPacket.Id — stable across flood relay hops, useful for duplicate-delivery suppression
	Text         string
}

// Session is a single live client-API connection to a Meshtastic device
// over TCP. Not safe to reconnect after Close; the caller owns
// reconnect-with-backoff by constructing a fresh Session.
type Session struct {
	conn    net.Conn
	fr      *frameReader
	writeMu sync.Mutex

	mu           sync.Mutex
	nodeNames    map[uint32]string // node number -> best-known display name
	channels     map[string]uint32 // normalized channel name -> local slot index
	havePrimary  bool              // whether we've seen a Channel_PRIMARY in the dump
	primaryIndex uint32            // that channel's index, valid iff havePrimary
	myNodeNum    uint32

	textCh   chan TextMessage
	closed   chan struct{}
	closeErr error
}

// Dial connects to a Meshtastic device's TCP client-API port (default 4403)
// and performs the want_config handshake, collecting the device's node
// database and configured channel slots before returning.
func Dial(addr string) (*Session, error) {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("meshtastic: dial %s: %w", addr, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	s := &Session{
		conn:      conn,
		fr:        newFrameReader(conn),
		nodeNames: make(map[uint32]string),
		channels:  make(map[string]uint32),
		textCh:    make(chan TextMessage, 32),
		closed:    make(chan struct{}),
	}

	if _, err := conn.Write(wakeSequence()); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("meshtastic: sending wake sequence: %w", err)
	}
	time.Sleep(100 * time.Millisecond)

	if err := s.handshake(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	go s.readLoop()
	return s, nil
}

// writeToRadio marshals and frames a ToRadio message to the device.
func (s *Session) writeToRadio(msg *generated.ToRadio) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("meshtastic: marshalling ToRadio: %w", err)
	}
	frame, err := encodeFrame(data)
	if err != nil {
		return fmt.Errorf("meshtastic: framing ToRadio: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.conn.Write(frame)
	return err
}

// handshake sends want_config and synchronously collects my_info,
// node_info, and channel messages until config_complete_id echoes our
// nonce. Any packet messages seen during the dump (unlikely but possible)
// are processed immediately rather than dropped.
func (s *Session) handshake() error {
	nonce := rand.Uint32()
	if nonce == 0 {
		nonce = 1
	}
	if err := s.writeToRadio(&generated.ToRadio{
		PayloadVariant: &generated.ToRadio_WantConfigId{WantConfigId: nonce},
	}); err != nil {
		return fmt.Errorf("meshtastic: sending want_config: %w", err)
	}

	deadline := time.Now().Add(DefaultCommandTimeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("meshtastic: timed out waiting for config_complete_id")
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(DefaultCommandTimeout))
		frame, err := s.fr.readFrame()
		if err != nil {
			return fmt.Errorf("meshtastic: reading handshake response: %w", err)
		}
		var fromRadio generated.FromRadio
		if err := proto.Unmarshal(frame, &fromRadio); err != nil {
			continue // ignore malformed frames rather than aborting the handshake
		}

		switch v := fromRadio.PayloadVariant.(type) {
		case *generated.FromRadio_MyInfo:
			if v.MyInfo != nil {
				s.myNodeNum = v.MyInfo.MyNodeNum
			}
		case *generated.FromRadio_NodeInfo:
			s.recordNodeInfo(v.NodeInfo)
		case *generated.FromRadio_Channel:
			s.recordChannel(v.Channel)
		case *generated.FromRadio_Packet:
			s.handlePacket(v.Packet)
		case *generated.FromRadio_ConfigCompleteId:
			if v.ConfigCompleteId == nonce {
				_ = s.conn.SetReadDeadline(time.Time{})
				return nil
			}
		default:
			// Config/ModuleConfig/LogRecord/Rebooted/etc: not needed here.
		}
	}
}

// readLoop runs after the handshake, delivering text messages and keeping
// the node/channel tables current as further updates arrive.
func (s *Session) readLoop() {
	for {
		frame, err := s.fr.readFrame()
		if err != nil {
			s.closeErr = err
			close(s.closed)
			return
		}
		var fromRadio generated.FromRadio
		if err := proto.Unmarshal(frame, &fromRadio); err != nil {
			continue
		}
		switch v := fromRadio.PayloadVariant.(type) {
		case *generated.FromRadio_Packet:
			s.handlePacket(v.Packet)
		case *generated.FromRadio_NodeInfo:
			s.recordNodeInfo(v.NodeInfo)
		case *generated.FromRadio_Channel:
			s.recordChannel(v.Channel)
		}
	}
}

// handlePacket extracts text messages (TEXT_MESSAGE_APP) for delivery and
// opportunistically updates the node DB from live NODEINFO_APP broadcasts.
func (s *Session) handlePacket(pkt *generated.MeshPacket) {
	if pkt == nil {
		return
	}
	decoded, ok := pkt.PayloadVariant.(*generated.MeshPacket_Decoded)
	if !ok || decoded.Decoded == nil {
		// Still MeshPacket_Encrypted: the attached device couldn't decrypt
		// it for us — usually means its channel PSK doesn't match the
		// sender's, or the sender is on a channel this device doesn't have
		// configured at all.
		logf("packet from node !%08x on channel %d arrived still encrypted (device could not decrypt it)", pkt.From, pkt.Channel)
		return
	}

	switch decoded.Decoded.Portnum {
	case generated.PortNum_TEXT_MESSAGE_APP:
		msg := TextMessage{
			From:         pkt.From,
			FromName:     s.resolveName(pkt.From),
			ChannelIndex: pkt.Channel,
			PacketID:     pkt.Id,
			Text:         string(decoded.Decoded.Payload),
		}
		logf("heard text from %s on channel %d: %q", msg.FromName, msg.ChannelIndex, msg.Text)
		select {
		case s.textCh <- msg:
		default: // drop if the consumer is falling behind
			logf("dropped text message from %s: consumer is falling behind", msg.FromName)
		}
	case generated.PortNum_NODEINFO_APP:
		var user generated.User
		if err := proto.Unmarshal(decoded.Decoded.Payload, &user); err == nil {
			s.recordUser(pkt.From, &user)
		}
	}
}

func (s *Session) recordNodeInfo(ni *generated.NodeInfo) {
	if ni == nil {
		return
	}
	s.recordUser(ni.Num, ni.User)
}

func (s *Session) recordUser(num uint32, user *generated.User) {
	if user == nil {
		return
	}
	name := strings.TrimSpace(user.LongName)
	if name == "" {
		name = strings.TrimSpace(user.ShortName)
	}
	if name == "" {
		return
	}
	s.mu.Lock()
	s.nodeNames[num] = name
	s.mu.Unlock()
}

func (s *Session) recordChannel(ch *generated.Channel) {
	if ch == nil || ch.Settings == nil {
		return
	}
	// Channel_DISABLED (the zero value — i.e. an unset Role too) marks an
	// unused slot. Skip it: disabled slots commonly have a blank name too,
	// and recording it would silently clobber the real primary channel's
	// entry in s.channels[""] with whichever disabled slot the device
	// happens to send last during the dump.
	if ch.Role == generated.Channel_DISABLED {
		return
	}
	norm := strings.ToLower(strings.TrimSpace(ch.Settings.Name))
	s.mu.Lock()
	s.channels[norm] = uint32(ch.Index)
	if ch.Role == generated.Channel_PRIMARY {
		s.havePrimary = true
		s.primaryIndex = uint32(ch.Index)
	}
	s.mu.Unlock()
}

// resolveName returns the node DB's best-known display name for num, or
// Meshtastic's conventional "!<8-hex-digit-node-id>" fallback if unknown.
func (s *Session) resolveName(num uint32) string {
	s.mu.Lock()
	name, ok := s.nodeNames[num]
	s.mu.Unlock()
	if ok && name != "" {
		return name
	}
	return fmt.Sprintf("!%08x", num)
}

// ResolveChannelIndex looks up the local channel slot index for a
// configured channel name against the attached device's own channel table
// (collected during the handshake and kept live afterwards). Falls back to
// index 0 for names conventionally associated with the primary channel
// (e.g. "LongFast", "primary", "") even if that slot's stored name is blank.
func (s *Session) ResolveChannelIndex(name string) (uint32, bool) {
	norm := strings.ToLower(strings.TrimSpace(name))
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx, ok := s.channels[norm]; ok {
		return idx, true
	}
	if wellKnownPrimaryNames[norm] {
		if s.havePrimary {
			return s.primaryIndex, true
		}
		// No definitive Channel_PRIMARY seen (unusual — the handshake
		// dump normally always includes one); guess index 0 rather than
		// failing outright.
		return 0, true
	}
	return 0, false
}

// SendText sends text on the named channel (resolved via
// ResolveChannelIndex) as a broadcast TEXT_MESSAGE_APP packet. The attached
// device performs that channel's encryption; delivery is fire-and-forget —
// this package does not track per-packet ACKs.
func (s *Session) SendText(channelName, text string) error {
	idx, ok := s.ResolveChannelIndex(channelName)
	if !ok {
		return fmt.Errorf("meshtastic: no channel slot on the attached device matches %q", channelName)
	}
	pkt := &generated.MeshPacket{
		To:      BroadcastAddr,
		Channel: idx,
		Id:      rand.Uint32(),
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_TEXT_MESSAGE_APP,
			Payload: []byte(text),
		}},
	}
	return s.writeToRadio(&generated.ToRadio{PayloadVariant: &generated.ToRadio_Packet{Packet: pkt}})
}

// TextMessages returns a channel of decoded TEXT_MESSAGE_APP packets heard
// on the mesh while this session is connected.
func (s *Session) TextMessages() <-chan TextMessage {
	return s.textCh
}

// MyNodeNum returns this session's own node number, valid after Dial.
func (s *Session) MyNodeNum() uint32 {
	return s.myNodeNum
}

// Done is closed when the session's read loop exits (connection closed or
// errored). Check Err() afterwards for the reason.
func (s *Session) Done() <-chan struct{} {
	return s.closed
}

// Err returns the error that caused the session to close, if any.
func (s *Session) Err() error {
	return s.closeErr
}

// Close closes the underlying connection.
func (s *Session) Close() error {
	return s.conn.Close()
}
