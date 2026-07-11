package bridge

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	generated "github.com/meshnet-gophers/meshtastic-go/meshtastic"
	"google.golang.org/protobuf/proto"

	"github.com/hectospark/hoplink/internal/config"
	"github.com/hectospark/hoplink/internal/discord"
	"github.com/hectospark/hoplink/internal/meshcore"
	"github.com/hectospark/hoplink/internal/meshtastic"
)

// The following minimal fake-radio plumbing duplicates (deliberately —
// these are unexported in package meshtastic) just enough of the client-API
// stream framing to drive a real *meshtastic.Session from this package's
// tests: 0x94/0xC3 magic + big-endian u16 length + protobuf bytes.

const (
	mtStart1 = 0x94
	mtStart2 = 0xC3
)

func mtReadFrame(r io.Reader) ([]byte, error) {
	br := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, br); err != nil {
			return nil, err
		}
		if br[0] != mtStart1 {
			continue
		}
		if _, err := io.ReadFull(r, br); err != nil {
			return nil, err
		}
		if br[0] != mtStart2 {
			continue
		}
		var hdr [2]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, err
		}
		length := binary.BigEndian.Uint16(hdr[:])
		body := make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
		return body, nil
	}
}

func mtEncodeFrame(data []byte) []byte {
	out := make([]byte, 4+len(data))
	out[0] = mtStart1
	out[1] = mtStart2
	binary.BigEndian.PutUint16(out[2:4], uint16(len(data)))
	copy(out[4:], data)
	return out
}

type mtFakeRadio struct {
	conn net.Conn
}

func (fr *mtFakeRadio) push(msg *generated.FromRadio) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fr.conn.Write(mtEncodeFrame(data))
	return err
}

// dialTestMeshtasticSession starts a fake Meshtastic device with two
// channels (primary/index 0 unnamed, "general" at index 1), dials a real
// *meshtastic.Session against it, and returns the session plus a channel of
// every ToRadio.Packet it sends and a handle to push unsolicited FromRadio
// messages (for simulating inbound mesh traffic).
func dialTestMeshtasticSession(t *testing.T) (*meshtastic.Session, chan *generated.MeshPacket, *mtFakeRadio) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	sentPackets := make(chan *generated.MeshPacket, 8)
	radioCh := make(chan *mtFakeRadio, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		radio := &mtFakeRadio{conn: conn}
		radioCh <- radio
		for {
			frame, err := mtReadFrame(conn)
			if err != nil {
				return
			}
			var toRadio generated.ToRadio
			if err := proto.Unmarshal(frame, &toRadio); err != nil {
				continue
			}
			switch v := toRadio.PayloadVariant.(type) {
			case *generated.ToRadio_WantConfigId:
				replies := []*generated.FromRadio{
					{PayloadVariant: &generated.FromRadio_MyInfo{MyInfo: &generated.MyNodeInfo{MyNodeNum: 1}}},
					{PayloadVariant: &generated.FromRadio_Channel{Channel: &generated.Channel{
						Index: 0, Role: generated.Channel_PRIMARY, Settings: &generated.ChannelSettings{Name: ""},
					}}},
					{PayloadVariant: &generated.FromRadio_Channel{Channel: &generated.Channel{
						Index: 1, Role: generated.Channel_SECONDARY, Settings: &generated.ChannelSettings{Name: "general"},
					}}},
					{PayloadVariant: &generated.FromRadio_ConfigCompleteId{ConfigCompleteId: v.WantConfigId}},
				}
				for _, r := range replies {
					_ = radio.push(r)
				}
			case *generated.ToRadio_Packet:
				sentPackets <- v.Packet
			}
		}
	}()

	session, err := meshtastic.Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("meshtastic.Dial: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	radio := <-radioCh
	return session, sentPackets, radio
}

func newTestMeshtasticMapping(t *testing.T, name, discordChannelID, meshtasticChannelName string) (*mapping, chan webhookPost) {
	t.Helper()
	posts := make(chan webhookPost, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content   string `json:"content"`
			Username  string `json:"username"`
			AvatarURL string `json:"avatar_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		posts <- webhookPost{username: body.Username, avatarURL: body.AvatarURL, content: body.Content}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	m := &mapping{
		cfg: config.Bridge{
			Name:              name,
			DiscordChannelID:  discordChannelID,
			DiscordWebhookURL: server.URL + "/api/webhooks/1/tok",
			Meshtastic:        config.BridgeMeshtastic{Enabled: true, ChannelName: meshtasticChannelName},
		},
		webhook:           discord.NewWebhookSender(server.URL+"/api/webhooks/1/tok", "test"),
		discordEnabled:    true,
		maxBytes:          320,
		meshtasticEnabled: true,
	}
	return m, posts
}

func TestBridge_HandleDiscordMessage_SendsToMeshtastic(t *testing.T) {
	session, sentPackets, _ := dialTestMeshtasticSession(t)
	m, _ := newTestMeshtasticMapping(t, "general", "chan-1", "general")
	b := newTestBridge(m)
	b.SetMeshtasticSession(session)

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  "chan-1",
		AuthorName: "Alice",
		Content:    "hello mesh",
	})

	select {
	case pkt := <-sentPackets:
		if pkt.Channel != 1 {
			t.Errorf("Channel = %d, want 1 (resolved index for \"general\")", pkt.Channel)
		}
		decoded, ok := pkt.PayloadVariant.(*generated.MeshPacket_Decoded)
		if !ok {
			t.Fatalf("PayloadVariant = %T, want Decoded", pkt.PayloadVariant)
		}
		if string(decoded.Decoded.Payload) != "Alice: hello mesh" {
			t.Errorf("payload = %q, want %q", decoded.Decoded.Payload, "Alice: hello mesh")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the fake radio to receive a packet")
	}
}

func TestBridge_HandleDiscordMessage_UnresolvedMeshtasticChannelLogsAndSkips(t *testing.T) {
	session, sentPackets, _ := dialTestMeshtasticSession(t)
	m, _ := newTestMeshtasticMapping(t, "general", "chan-1", "does-not-exist")
	b := newTestBridge(m)
	b.SetMeshtasticSession(session)

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  "chan-1",
		AuthorName: "Alice",
		Content:    "hello mesh",
	})

	select {
	case pkt := <-sentPackets:
		t.Fatalf("unexpected send for an unresolvable channel name: %+v", pkt)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBridge_HandleDiscordMessage_SkipsMeshtasticSendWhenSessionNotAttached(t *testing.T) {
	m, _ := newTestMeshtasticMapping(t, "general", "chan-1", "general")
	b := newTestBridge(m) // no meshtastic session attached

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  "chan-1",
		AuthorName: "Alice",
		Content:    "hello mesh",
	})
	// Must not panic; nothing further to assert.
}

func TestBridge_HandleMeshtasticMessage_PostsUnderNodeDBName(t *testing.T) {
	session, _, radio := dialTestMeshtasticSession(t)
	m, posts := newTestMeshtasticMapping(t, "general", "chan-1", "general")
	b := newTestBridge(m)
	b.SetMeshtasticSession(session)

	// First, tell the session about node 42's identity (as a live
	// NODEINFO_APP broadcast, matching how a real device streams updates).
	userBytes, err := proto.Marshal(&generated.User{LongName: "Bob Mesh"})
	if err != nil {
		t.Fatalf("marshal User: %v", err)
	}
	_ = radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From: 42,
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_NODEINFO_APP,
			Payload: userBytes,
		}},
	}}})
	time.Sleep(50 * time.Millisecond)

	_ = radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From:    42,
		Channel: 1, // "general"
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_TEXT_MESSAGE_APP,
			Payload: []byte("hi from the mesh"),
		}},
	}}})
	pumpOneTextMessage(t, b, session)

	select {
	case p := <-posts:
		if p.username != "Bob Mesh" || p.content != "hi from the mesh" {
			t.Errorf("got post %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook post")
	}
}

func TestBridge_HandleMeshtasticMessage_SuppressesOwnEcho(t *testing.T) {
	session, _, radio := dialTestMeshtasticSession(t)
	m, posts := newTestMeshtasticMapping(t, "general", "chan-1", "general")
	b := newTestBridge(m)
	b.SetMeshtasticSession(session)

	b.markOutboundSent(meshtasticEchoKey(1, "Alice: from discord"))
	_ = radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From:    1,
		Channel: 1,
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_TEXT_MESSAGE_APP,
			Payload: []byte("Alice: from discord"),
		}},
	}}})
	pumpOneTextMessage(t, b, session)

	select {
	case p := <-posts:
		t.Fatalf("expected our own echoed send to be suppressed, got post: %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestBridge_HandleMeshtasticMessage_SuppressesDuplicateFloodedPacket(t *testing.T) {
	session, _, radio := dialTestMeshtasticSession(t)
	m, posts := newTestMeshtasticMapping(t, "general", "chan-1", "general")
	b := newTestBridge(m)
	b.SetMeshtasticSession(session)

	pkt := &generated.MeshPacket{
		From: 7, Channel: 1, Id: 999,
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_TEXT_MESSAGE_APP,
			Payload: []byte("hi again"),
		}},
	}
	_ = radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: pkt}})
	pumpOneTextMessage(t, b, session)
	_ = radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: pkt}}) // duplicate, same packet ID
	pumpOneTextMessage(t, b, session)

	select {
	case <-posts:
	case <-time.After(2 * time.Second):
		t.Fatal("expected exactly one post")
	}
	select {
	case p := <-posts:
		t.Fatalf("expected the duplicate to be suppressed, got a second post: %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

// pumpOneTextMessage reads exactly one message from session.TextMessages()
// and dispatches it through the bridge, mirroring what Bridge.RunMeshtastic
// would do — used directly in tests instead of running the full Run loop,
// the same style already used for MeshCore's handleMeshcorePacket tests.
func pumpOneTextMessage(t *testing.T, b *Bridge, session *meshtastic.Session) {
	t.Helper()
	select {
	case msg := <-session.TextMessages():
		b.handleMeshtasticMessage(session, msg)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a text message from the session")
	}
}

func TestBridge_HandleDiscordMessage_DualBackendSendsToBoth(t *testing.T) {
	mcSession, mcPackets := dialTestSession(t)
	mtSession, mtPackets, _ := dialTestMeshtasticSession(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	secret := meshcore.HashtagChannelSecret("#general")
	chHash, err := meshcore.ChannelHash(secret)
	if err != nil {
		t.Fatalf("ChannelHash: %v", err)
	}
	m := &mapping{
		cfg: config.Bridge{
			Name:              "general",
			DiscordChannelID:  "chan-1",
			DiscordWebhookURL: server.URL + "/api/webhooks/1/tok",
			Meshtastic:        config.BridgeMeshtastic{Enabled: true, ChannelName: "general"},
		},
		webhook:           discord.NewWebhookSender(server.URL+"/api/webhooks/1/tok", "test"),
		discordEnabled:    true,
		maxBytes:          320,
		meshcoreEnabled:   true,
		meshtasticEnabled: true,
		secret:            secret,
		channelHash:       chHash,
	}
	b := newTestBridge(m)
	b.SetMeshcoreSession(mcSession)
	b.SetMeshtasticSession(mtSession)
	b.route = meshcore.RouteFlood
	b.hashSize = 3

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  "chan-1",
		AuthorName: "Alice",
		Content:    "hello both",
	})

	select {
	case <-mcPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the meshcore send")
	}
	select {
	case pkt := <-mtPackets:
		decoded := pkt.PayloadVariant.(*generated.MeshPacket_Decoded)
		if string(decoded.Decoded.Payload) != "Alice: hello both" {
			t.Errorf("meshtastic payload = %q, want %q", decoded.Decoded.Payload, "Alice: hello both")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the meshtastic send")
	}
}
