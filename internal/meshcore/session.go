package meshcore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

func logf(format string, args ...any) {
	log.Printf("[meshcore] "+format, args...)
}

// DefaultCommandTimeout is the per-command response wait, per
// docs/companion_protocol.md's recommended timeout.
const DefaultCommandTimeout = 5 * time.Second

// keepAliveInterval is how often Session pings the radio when otherwise
// idle. Companion TCP connections routinely get dropped by NAT/router
// idle-connection timeouts (commonly ~120s) when no traffic flows for a
// while; comfortably undercutting that keeps the mapping alive.
const keepAliveInterval = 60 * time.Second

// tcpKeepAlivePeriod is the OS-level TCP keepalive probe interval set on the
// underlying socket, belt-and-braces alongside the application-level ping
// (some middleboxes track actual data packets, not bare ACKs).
const tcpKeepAlivePeriod = 30 * time.Second

// syncPollInterval is how often syncLoop drains queued channel messages via
// CMD_SYNC_NEXT_MESSAGE even without a PUSH_CODE_MSG_WAITING notification —
// a safety net in case that push was itself missed (the same class of
// problem this whole mechanism exists to route around for GRP_TXT
// messages), so a message queued on the device is never stuck waiting on a
// single dropped "tickle". A var (not const) so tests can shorten it.
var syncPollInterval = 10 * time.Second

// SelfInfo is the parsed reply to CMD_APP_START (RESP_CODE_SELF_INFO / 0x05).
type SelfInfo struct {
	PublicKey [32]byte
	Name      string
	RadioFreq float64 // MHz
	RadioBW   float64 // kHz
	RadioSF   uint8
	RadioCR   uint8
}

func parseSelfInfo(data []byte) (SelfInfo, error) {
	// Layout per docs/companion_protocol.md:
	// [0]=0x05 [1]adv_type [2]tx_power [3]max_tx_power [4:36]pubkey
	// [36:40]lat [40:44]lon [44]multi_acks [45]adv_loc_policy [46]telemetry
	// [47]manual_add [48:52]freq [52:56]bw [56]sf [57]cr [58:]name
	if len(data) < 58 {
		return SelfInfo{}, fmt.Errorf("meshcore: SELF_INFO frame too short (%d bytes)", len(data))
	}
	var info SelfInfo
	copy(info.PublicKey[:], data[4:36])
	info.RadioFreq = float64(binary.LittleEndian.Uint32(data[48:52])) / 1000.0
	info.RadioBW = float64(binary.LittleEndian.Uint32(data[52:56])) / 1000.0
	info.RadioSF = data[56]
	info.RadioCR = data[57]
	if len(data) > 58 {
		info.Name = trimNulAndSpace(string(data[58:]))
	}
	return info, nil
}

func trimNulAndSpace(s string) string {
	end := len(s)
	for end > 0 && (s[end-1] == 0 || s[end-1] == ' ') {
		end--
	}
	start := 0
	for start < end && s[start] == ' ' {
		start++
	}
	return s[start:end]
}

// ErrResponse is returned when the radio replies with FrameErr (0x01).
type ErrResponse struct {
	Code byte
}

func (e *ErrResponse) Error() string {
	return fmt.Sprintf("meshcore: radio returned error code %d", e.Code)
}

// Session is a single live connection to a MeshCore companion radio over
// its TCP interface. It is not safe to reconnect a Session after Close; the
// caller (e.g. the bridge orchestrator) owns reconnect-with-backoff by
// constructing a fresh Session.
type Session struct {
	conn net.Conn
	fr   *FrameReader

	cmdMu        sync.Mutex // serialises whole request/response cycles (write + wait) across all callers, including the keepalive loop and the sync-drain loop
	writeCh      chan writeReq
	respCh       chan []byte
	logRxCh      chan LogRxData
	channelMsgCh chan ChannelMessage // decoded via CMD_SYNC_NEXT_MESSAGE — see RegisterChannel/syncLoop
	syncTrigger  chan struct{}       // signalled by readLoop on PUSH_CODE_MSG_WAITING; always has a real consumer (syncLoop)
	pushCh       chan []byte
	// pushConsumed tracks whether PushFrames has ever been called. Nobody in
	// this codebase currently calls it — it exists for callers that need
	// push codes LogRxFrames doesn't decode (e.g. PUSH_CODE_SEND_CONFIRMED)
	// — so writing to (and logging drops for) pushCh before anyone has ever
	// asked for it would just be constant, misleading noise: a channel with
	// zero consumers is *always* "full", not evidence anything real was lost.
	pushConsumed atomic.Bool
	closed       chan struct{}
	closeErr     error
}

type writeReq struct {
	frame []byte
	err   chan error
}

// Dial connects to a MeshCore companion radio's TCP server at addr
// ("host:port") and performs the CMD_APP_START handshake, identifying this
// client as appName. It returns once SELF_INFO has been received.
func Dial(addr, appName string) (*Session, SelfInfo, error) {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, SelfInfo{}, fmt.Errorf("meshcore: dial %s: %w", addr, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(tcpKeepAlivePeriod)
	}
	return newSessionOverConn(conn, appName)
}

// newSessionOverConn wraps an already-open connection (real TCP, or a
// net.Pipe in tests) and performs the CMD_APP_START handshake.
func newSessionOverConn(conn net.Conn, appName string) (*Session, SelfInfo, error) {
	s := &Session{
		conn:         conn,
		fr:           NewFrameReader(conn),
		writeCh:      make(chan writeReq),
		respCh:       make(chan []byte, 1),
		logRxCh:      make(chan LogRxData, 32),
		channelMsgCh: make(chan ChannelMessage, 32),
		syncTrigger:  make(chan struct{}, 1),
		pushCh:       make(chan []byte, 32),
		closed:       make(chan struct{}),
	}
	go s.writeLoop()
	go s.readLoop()

	nameBytes := []byte(appName)
	appStart := make([]byte, 8+len(nameBytes))
	appStart[0] = CmdAppStart
	copy(appStart[8:], nameBytes)

	resp, err := s.request(appStart, []byte{FrameSelfInfo, FrameErr}, DefaultCommandTimeout)
	if err != nil {
		_ = s.Close()
		return nil, SelfInfo{}, fmt.Errorf("meshcore: APP_START handshake: %w", err)
	}
	if len(resp) == 0 || resp[0] != FrameSelfInfo {
		_ = s.Close()
		return nil, SelfInfo{}, fmt.Errorf("meshcore: APP_START handshake: unexpected reply frame %v", resp)
	}
	info, err := parseSelfInfo(resp)
	if err != nil {
		_ = s.Close()
		return nil, SelfInfo{}, fmt.Errorf("meshcore: APP_START handshake: %w", err)
	}
	go s.keepAliveLoop()
	go s.syncLoop()
	return s, info, nil
}

// keepAliveLoop periodically pings the radio (CMD_GET_DEVICE_TIME, a
// side-effect-free query) so the TCP connection sees regular traffic even
// on a quiet mesh channel, preventing idle-connection drops. Ping failures
// are ignored here: readLoop will observe the same dead connection and
// close Session, which the caller (bridge/main) reconnects.
func (s *Session) keepAliveLoop() {
	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_, _ = s.request([]byte{CmdGetDeviceTime}, []byte{FrameCurrTime, FrameErr}, DefaultCommandTimeout)
		case <-s.closed:
			return
		}
	}
}

// writeLoop serialises writes to the connection (mirrors the companion
// protocol's single-in-flight-command model).
func (s *Session) writeLoop() {
	for {
		select {
		case req := <-s.writeCh:
			_, err := s.conn.Write(EncodeFrame(req.frame))
			req.err <- err
		case <-s.closed:
			return
		}
	}
}

// readLoop continuously reads frames and dispatches them: frames with a
// first byte >= 0x80 are push notifications, everything else is treated as
// a synchronous command response.
func (s *Session) readLoop() {
	for {
		frame, err := s.fr.ReadFrame()
		if err != nil {
			s.closeErr = err
			close(s.closed)
			return
		}
		if len(frame) == 0 {
			continue
		}
		if frame[0] >= 0x80 {
			if frame[0] == PushLogRxData {
				if lrx, err := ParseLogRxData(frame); err == nil {
					select {
					case s.logRxCh <- lrx:
					default:
						logf("dropped LogRxData push: consumer is falling behind")
					}
				}
			}
			if frame[0] == PushMsgWaiting {
				// Always has a real consumer (syncLoop) — unlike the generic
				// pushCh forwarding below, this is never optional.
				select {
				case s.syncTrigger <- struct{}{}:
				default: // a drain is already pending/in progress; it'll pick up this message too
				}
			}
			if s.pushConsumed.Load() {
				select {
				case s.pushCh <- frame:
				default:
					logf("dropped push frame %#x: nobody listening", frame[0])
				}
			}
			continue
		}
		select {
		case s.respCh <- frame:
		default:
			// FrameCurrTime is the keepalive's own expected reply; if it
			// arrives after keepAliveLoop's request() already gave up
			// waiting (a slow radio, not a bug), it lands here every cycle
			// and isn't worth logging. Anything else arriving with nobody
			// waiting is genuinely unexpected and worth surfacing.
			if frame[0] != FrameCurrTime {
				logf("dropped stray response frame %#x: nobody waiting", frame[0])
			}
		}
	}
}

// request writes frame and waits up to timeout for a response frame whose
// first byte is one of wantCodes. cmdMu serialises the full write+wait cycle
// across all callers (bridge sends, the keepalive loop, ...) so two
// concurrent requests can never race for the same response frame — the
// companion protocol is not designed for pipelining.
//
// Any frame arriving that doesn't match wantCodes is discarded rather than
// returned: the radio can emit a frame that isn't the answer to what we just
// asked (e.g. a delayed reply to an earlier command whose own wait already
// timed out), and misattributing it to the current caller silently breaks
// unrelated sends. We keep waiting — up to the same overall timeout — for a
// frame that actually matches.
func (s *Session) request(frame []byte, wantCodes []byte, timeout time.Duration) ([]byte, error) {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	errCh := make(chan error, 1)
	select {
	case s.writeCh <- writeReq{frame: frame, err: errCh}:
	case <-s.closed:
		return nil, s.closeErrOrDefault()
	}
	if err := <-errCh; err != nil {
		return nil, err
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case resp := <-s.respCh:
			if len(resp) > 0 && containsByte(wantCodes, resp[0]) {
				return resp, nil
			}
			// Stray/unexpected frame: discard and keep waiting.
			if len(resp) > 0 {
				logf("discarding stray response frame %#x while waiting for one of %v", resp[0], wantCodes)
			}
			continue
		case <-s.closed:
			return nil, s.closeErrOrDefault()
		case <-deadline.C:
			return nil, fmt.Errorf("meshcore: timed out waiting for response to command %#x", frame[0])
		}
	}
}

func containsByte(codes []byte, b byte) bool {
	for _, c := range codes {
		if c == b {
			return true
		}
	}
	return false
}

func (s *Session) closeErrOrDefault() error {
	if s.closeErr != nil {
		return s.closeErr
	}
	return fmt.Errorf("meshcore: session closed")
}

// checkOK interprets a command-response frame as either FrameOK (success) or
// FrameErr (returning *ErrResponse).
func checkOK(resp []byte) error {
	if len(resp) == 0 {
		return fmt.Errorf("meshcore: empty response frame")
	}
	switch resp[0] {
	case FrameOK:
		return nil
	case FrameErr:
		code := byte(0)
		if len(resp) > 1 {
			code = resp[1]
		}
		return &ErrResponse{Code: code}
	default:
		return fmt.Errorf("meshcore: unexpected response frame code %#x", resp[0])
	}
}

// SendRawPacket transmits a fully app-composed RF packet via
// CMD_SEND_RAW_PACKET (65) and waits for the radio's OK/ERR acknowledgement.
func (s *Session) SendRawPacket(packet []byte, priority byte) error {
	frame, err := BuildSendRawPacketFrame(packet, priority)
	if err != nil {
		return err
	}
	resp, err := s.request(frame, []byte{FrameOK, FrameErr}, DefaultCommandTimeout)
	if err != nil {
		return err
	}
	return checkOK(resp)
}

// SendChannelMessage is a convenience wrapper: derives the RF packet for a
// GRP_TXT channel message and sends it via SendRawPacket.
//
// hashSize sets the path hash width (1-3 bytes/hop) that relays will use
// when they extend this packet's path while flooding it — repeaters read it
// straight off this packet's own path_len byte (firmware Mesh.cpp,
// Packet::getPathHashSize), not from any device-level preference, since a
// raw packet bypasses the device's own CMD_SET_PATH_HASH_MODE default
// entirely. It applies even though the outgoing path itself starts empty
// (hopCount 0): the encoded hash-size bits still travel with the packet.
//
// scopeKey16, when non-nil, scopes the packet to a named flood
// scope/region: the route is upgraded to its TRANSPORT_* variant and
// transport_codes[0] is set via CalcTransportCode. Pass nil for the
// legacy unscoped behaviour (plain ROUTE_TYPE_FLOOD/_DIRECT).
func (s *Session) SendChannelMessage(secret16 []byte, route RfRouteType, hashSize int, scopeKey16 []byte, text string) error {
	payload, err := BuildGroupTextPayload(secret16, uint32(time.Now().Unix()), 0, text)
	if err != nil {
		return err
	}

	pkt := Packet{
		Route:       route,
		PayloadType: PayloadTypeGrpTxt,
		Version:     PayloadVer1,
		HashSize:    hashSize,
		Payload:     payload,
	}
	if len(scopeKey16) > 0 {
		code0, err := CalcTransportCode(scopeKey16, PayloadTypeGrpTxt, payload)
		if err != nil {
			return err
		}
		pkt.Route = route.WithTransportCodes()
		pkt.TransportCodes = EncodeTransportCodes(code0, 0)
	}

	packet, err := BuildPacket(pkt)
	if err != nil {
		return err
	}
	return s.SendRawPacket(packet, 0)
}

// ChannelMessage is a channel/group text message retrieved via
// CMD_SYNC_NEXT_MESSAGE — the device already decrypted it using the secret
// registered (via RegisterChannel) at SlotIndex. This is a second,
// independent inbound path alongside LogRxFrames: the device's own queue
// (backed by firmware's addToOfflineQueue) persists across this process's
// momentary hiccups in a way a local Go channel buffer alone cannot, so a
// message is only lost if *both* paths miss it.
type ChannelMessage struct {
	SlotIndex     byte
	TimestampUnix uint32
	Text          string
}

// RegisterChannel ensures secret16 is registered on the device under name,
// so the device will decrypt and queue GRP_TXT messages on that channel for
// retrieval via ChannelMessages/CMD_SYNC_NEXT_MESSAGE. Reuses an existing
// slot already holding this exact secret (idempotent across reconnects);
// otherwise claims the first empty slot (1-MaxChannelSlots; index 0 is
// conventionally reserved for the public channel, see MaxChannelSlots).
// Never touches a slot holding a *different* secret — this must not clobber
// another tool's channel registration on a shared device. Returns an error
// if every slot is occupied by an unrelated channel.
func (s *Session) RegisterChannel(secret16 []byte, name string) (byte, error) {
	if len(secret16) != 16 {
		return 0, fmt.Errorf("meshcore: channel secret must be exactly 16 bytes, got %d", len(secret16))
	}

	emptySlot := -1
	for idx := byte(1); idx <= MaxChannelSlots; idx++ {
		resp, err := s.request(BuildGetChannelFrame(idx), []byte{FrameChannelInfo, FrameErr}, DefaultCommandTimeout)
		if err != nil {
			return 0, fmt.Errorf("meshcore: getting channel slot %d: %w", idx, err)
		}
		if len(resp) > 0 && resp[0] == FrameErr {
			continue
		}
		info, ok := ParseChannelInfo(resp)
		if !ok {
			continue
		}
		if bytes.Equal(info.Secret, secret16) {
			return idx, nil // already registered from a prior run
		}
		if emptySlot == -1 && info.IsEmptyChannelSlot() {
			emptySlot = int(idx)
		}
	}
	if emptySlot == -1 {
		return 0, fmt.Errorf("meshcore: no free channel slot for %q (all %d slots in use)", name, MaxChannelSlots)
	}

	frame, err := BuildSetChannelFrame(byte(emptySlot), name, secret16)
	if err != nil {
		return 0, err
	}
	resp, err := s.request(frame, []byte{FrameOK, FrameErr}, DefaultCommandTimeout)
	if err != nil {
		return 0, err
	}
	if err := checkOK(resp); err != nil {
		return 0, fmt.Errorf("meshcore: registering channel %q at slot %d: %w", name, emptySlot, err)
	}
	return byte(emptySlot), nil
}

// syncLoop drains queued channel messages via CMD_SYNC_NEXT_MESSAGE,
// woken by either a PUSH_CODE_MSG_WAITING notification (readLoop signals
// syncTrigger) or syncPollInterval (a safety net in case that push was
// itself missed).
func (s *Session) syncLoop() {
	ticker := time.NewTicker(syncPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.syncTrigger:
			s.drainMessages()
		case <-ticker.C:
			s.drainMessages()
		case <-s.closed:
			return
		}
	}
}

// drainMessages repeatedly issues CMD_SYNC_NEXT_MESSAGE until the device
// reports PACKET_NO_MORE_MSGS, so a single wake clears the *entire*
// backlog, not just one message. Channel messages are emitted on
// ChannelMessages (non-blocking, dropped+logged if the consumer's behind,
// mirroring LogRxFrames' own pattern); contact (direct) messages and
// channel data datagrams are logged and otherwise ignored — out of scope,
// matching what the raw-log path already does (GRP_TXT/channel-text only).
func (s *Session) drainMessages() {
	for {
		resp, err := s.request(BuildSyncNextMessageFrame(), []byte{
			FrameChannelMsgRecv, FrameChannelMsgRecvV3,
			FrameContactMsgRecv, FrameContactMsgRecvV3,
			FrameChannelDataRecv, FrameNoMoreMessages, FrameErr,
		}, DefaultCommandTimeout)
		if err != nil {
			logf("sync: CMD_SYNC_NEXT_MESSAGE failed: %v", err)
			return
		}
		if len(resp) == 0 {
			return
		}
		switch resp[0] {
		case FrameNoMoreMessages:
			return
		case FrameChannelMsgRecv, FrameChannelMsgRecvV3:
			msg, ok := ParseChannelMsgRecv(resp)
			if !ok {
				logf("sync: malformed channel message frame %#x", resp[0])
				continue
			}
			cm := ChannelMessage{SlotIndex: msg.ChannelIndex, TimestampUnix: msg.TimestampUnix, Text: msg.Text}
			select {
			case s.channelMsgCh <- cm:
			default:
				logf("dropped synced channel message (slot %d): consumer is falling behind", cm.SlotIndex)
			}
		case FrameContactMsgRecv, FrameContactMsgRecvV3:
			logf("sync: ignoring contact (direct) message — not supported")
		case FrameChannelDataRecv:
			logf("sync: ignoring channel data datagram — not supported")
		case FrameErr:
			logf("sync: CMD_SYNC_NEXT_MESSAGE returned an error")
			return
		default:
			return
		}
	}
}

// ChannelMessages returns a channel of decoded channel/group text messages
// retrieved via CMD_SYNC_NEXT_MESSAGE — see RegisterChannel and
// ChannelMessage's own doc comment for how this relates to LogRxFrames.
func (s *Session) ChannelMessages() <-chan ChannelMessage {
	return s.channelMsgCh
}

// LogRxFrames returns a channel of decoded PUSH_CODE_LOG_RX_DATA (0x88)
// frames — every RF packet the radio hears while this session is connected.
func (s *Session) LogRxFrames() <-chan LogRxData {
	return s.logRxCh
}

// PushFrames returns a channel of all raw push frames (first byte >= 0x80),
// including ones LogRxFrames already decodes, for callers that need e.g.
// PUSH_CODE_SEND_CONFIRMED (0x82) or PUSH_CODE_MSG_WAITING (0x83). Frames
// are only forwarded here (and drops only logged) once this method has been
// called at least once — see pushConsumed.
func (s *Session) PushFrames() <-chan []byte {
	s.pushConsumed.Store(true)
	return s.pushCh
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
