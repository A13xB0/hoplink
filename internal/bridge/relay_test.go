package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	generated "github.com/meshnet-gophers/meshtastic-go/meshtastic"

	"github.com/hectospark/hoplink/internal/config"
	"github.com/hectospark/hoplink/internal/discord"
	"github.com/hectospark/hoplink/internal/meshcore"
)

// newDualBackendMapping builds a mapping with both MeshCore and Meshtastic
// enabled, backed by a real httptest webhook server — used to test direct
// MeshCore<->Meshtastic relaying. The caller attaches whichever fake
// sessions it needs via b.SetMeshcoreSession/SetMeshtasticSession.
func newDualBackendMapping(t *testing.T, senderFormat string) (*mapping, chan webhookPost) {
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
			SenderFormat:      senderFormat,
			Meshtastic:        config.BridgeMeshtastic{Enabled: true, ChannelName: "general"},
		},
		webhook:           discord.NewWebhookSender(server.URL+"/api/webhooks/1/tok", "test"),
		discordEnabled:    true,
		maxBytes:          320,
		senderFormat:      senderFormat,
		meshcoreEnabled:   true,
		meshtasticEnabled: true,
		secret:            secret,
		channelHash:       chHash,
	}
	return m, posts
}

func TestBridge_HandleMeshcorePacket_RelaysToMeshtasticWhenBothEnabled(t *testing.T) {
	mtSession, mtPackets, _ := dialTestMeshtasticSession(t)
	m, posts := newDualBackendMapping(t, "none")
	b := newTestBridge(m)
	b.SetMeshtasticSession(mtSession)

	lrx := buildLogRxData(t, m.secret, 1000, "Alice: hello from meshcore")
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-posts:
		if p.username != "Alice" || p.content != "hello from meshcore" {
			t.Errorf("discord post = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the discord webhook post")
	}

	select {
	case pkt := <-mtPackets:
		decoded, ok := pkt.PayloadVariant.(*generated.MeshPacket_Decoded)
		if !ok {
			t.Fatalf("PayloadVariant = %T, want Decoded", pkt.PayloadVariant)
		}
		if string(decoded.Decoded.Payload) != "Alice: hello from meshcore" {
			t.Errorf("meshtastic payload = %q, want %q", decoded.Decoded.Payload, "Alice: hello from meshcore")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the meshcore->meshtastic relay")
	}
}

func TestBridge_HandleMeshcorePacket_RelayUsesSenderFormat(t *testing.T) {
	mtSession, mtPackets, _ := dialTestMeshtasticSession(t)
	m, posts := newDualBackendMapping(t, "short")
	b := newTestBridge(m)
	b.SetMeshtasticSession(mtSession)

	lrx := buildLogRxData(t, m.secret, 1000, "Alice: hi")
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-posts:
		if p.username != "Alice (MC)" {
			t.Errorf("discord username = %q, want %q", p.username, "Alice (MC)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the discord webhook post")
	}

	select {
	case pkt := <-mtPackets:
		decoded := pkt.PayloadVariant.(*generated.MeshPacket_Decoded)
		if string(decoded.Decoded.Payload) != "Alice (MC): hi" {
			t.Errorf("meshtastic payload = %q, want %q", decoded.Decoded.Payload, "Alice (MC): hi")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the meshcore->meshtastic relay")
	}
}

func TestBridge_HandleMeshcorePacket_DoesNotRelayWhenMeshtasticDisabled(t *testing.T) {
	m, _ := newTestMapping(t, "general", "#general") // meshcore-only mapping
	b := newTestBridge(m)
	// No meshtastic session attached at all; must not panic or attempt a send.
	lrx := buildLogRxData(t, m.secret, 1000, "Alice: hi")
	b.handleMeshcorePacket(lrx)
}

func TestBridge_HandleMeshtasticMessage_RelaysToMeshcoreWhenBothEnabled(t *testing.T) {
	mcSession, mcPackets := dialTestSession(t)
	m, posts := newDualBackendMapping(t, "none")
	b := newTestBridge(m)
	b.SetMeshcoreSession(mcSession)
	b.route = meshcore.RouteFlood
	b.hashSize = 3

	mtSession, _, radio := dialTestMeshtasticSession(t)
	b.SetMeshtasticSession(mtSession)

	radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From:    42,
		Channel: 1, // "general"
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_TEXT_MESSAGE_APP,
			Payload: []byte("hello from meshtastic"),
		}},
	}}})
	pumpOneTextMessage(t, b, mtSession)

	select {
	case p := <-posts:
		if p.content != "hello from meshtastic" {
			t.Errorf("discord post = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the discord webhook post")
	}

	select {
	case raw := <-mcPackets:
		pkt, err := meshcore.ParsePacket(raw)
		if err != nil {
			t.Fatalf("ParsePacket: %v", err)
		}
		dec, ok := meshcore.DecryptGroupText(m.secret, pkt.Payload)
		if !ok {
			t.Fatal("DecryptGroupText failed on the relayed packet")
		}
		if dec.Text != "!0000002a: hello from meshtastic" {
			t.Errorf("meshcore text = %q, want %q", dec.Text, "!0000002a: hello from meshtastic")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the meshtastic->meshcore relay")
	}
}

func TestBridge_HandleMeshtasticMessage_DoesNotRelayWhenMeshcoreDisabled(t *testing.T) {
	mtSession, _, radio := dialTestMeshtasticSession(t)
	m, _ := newTestMeshtasticMapping(t, "general", "chan-1", "general") // meshtastic-only mapping
	b := newTestBridge(m)
	b.SetMeshtasticSession(mtSession)
	// No meshcore session attached at all; must not panic or attempt a send.

	radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From:    1,
		Channel: 1,
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_TEXT_MESSAGE_APP,
			Payload: []byte("hi"),
		}},
	}}})
	pumpOneTextMessage(t, b, mtSession)
}

func TestBridge_RelayedMessage_SelfEchoDoesNotLoopBackAcrossBackends(t *testing.T) {
	// A MeshCore message relayed to Meshtastic marks the composed text as
	// our own outbound send on the meshtastic side; when the mesh floods
	// that same packet back to us, it must be suppressed there, not
	// re-relayed back to MeshCore or re-posted to Discord a second time.
	mtSession, mtPackets, radio := dialTestMeshtasticSession(t)
	m, posts := newDualBackendMapping(t, "none")
	b := newTestBridge(m)
	b.SetMeshtasticSession(mtSession)

	lrx := buildLogRxData(t, m.secret, 1000, "Alice: hi")
	b.handleMeshcorePacket(lrx)

	select {
	case <-posts:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the initial discord webhook post")
	}

	var relayedPkt *generated.MeshPacket
	select {
	case relayedPkt = <-mtPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the meshcore->meshtastic relay")
	}

	// Simulate the mesh flooding our own relayed send back to us.
	decoded := relayedPkt.PayloadVariant.(*generated.MeshPacket_Decoded)
	radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From:    1, // our own node
		Channel: 1,
		Id:      relayedPkt.Id,
		PayloadVariant: &generated.MeshPacket_Decoded{
			Decoded: &generated.Data{Portnum: generated.PortNum_TEXT_MESSAGE_APP, Payload: decoded.Decoded.Payload},
		},
	}}})
	pumpOneTextMessage(t, b, mtSession)

	select {
	case p := <-posts:
		t.Fatalf("expected the relayed message's own echo to be suppressed, got a second post: %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}
