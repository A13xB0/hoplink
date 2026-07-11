package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hectospark/hoplink/internal/config"
	"github.com/hectospark/hoplink/internal/discord"
	"github.com/hectospark/hoplink/internal/meshcore"
)

// newTestMapping builds a mapping backed by a real httptest webhook server,
// returning the mapping and a capture channel of (username, avatarURL, content) posts.
type webhookPost struct {
	username, avatarURL, content string
}

func newTestMapping(t *testing.T, name, hashtag string) (*mapping, chan webhookPost) {
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

	bc := config.Bridge{
		Name:              name,
		DiscordChannelID:  "chan-" + name,
		DiscordWebhookURL: server.URL + "/api/webhooks/1/tok",
		MeshCore:          config.BridgeMeshCore{Enabled: true, Hashtag: hashtag},
	}
	secret, err := bc.Secret()
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	chHash, err := meshcore.ChannelHash(secret)
	if err != nil {
		t.Fatalf("ChannelHash: %v", err)
	}
	return &mapping{
		cfg:             bc,
		secret:          secret,
		channelHash:     chHash,
		webhook:         discord.NewWebhookSender(server.URL + "/api/webhooks/1/tok"),
		maxBytes:        320,
		meshcoreEnabled: true,
	}, posts
}

func newTestBridge(mappings ...*mapping) *Bridge {
	b := &Bridge{
		byChan:         make(map[string]*mapping),
		recentInbound:  make(map[string]time.Time),
		recentOutbound: make(map[string]time.Time),
	}
	for _, m := range mappings {
		b.byName = append(b.byName, m)
		b.byChan[m.cfg.DiscordChannelID] = m
	}
	return b
}

func buildLogRxData(t *testing.T, secret []byte, timestamp uint32, text string) meshcore.LogRxData {
	t.Helper()
	payload, err := meshcore.BuildGroupTextPayload(secret, timestamp, 0, text)
	if err != nil {
		t.Fatalf("BuildGroupTextPayload: %v", err)
	}
	pkt, err := meshcore.BuildPacket(meshcore.Packet{
		Route:       meshcore.RouteFlood,
		PayloadType: meshcore.PayloadTypeGrpTxt,
		Payload:     payload,
	})
	if err != nil {
		t.Fatalf("BuildPacket: %v", err)
	}
	parsed, err := meshcore.ParsePacket(pkt)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	return meshcore.LogRxData{SNR: -5, RSSI: -80, Packet: parsed}
}

func TestBridge_HandleMeshcorePacket_PostsUnderSenderName(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)

	lrx := buildLogRxData(t, m.secret, 1000, "Alice: hello from the mesh")
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-posts:
		if p.username != "Alice" || p.content != "hello from the mesh" {
			t.Errorf("got post %+v", p)
		}
		if p.avatarURL != avatarURLForName("Alice") {
			t.Errorf("avatarURL = %q, want %q", p.avatarURL, avatarURLForName("Alice"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook post")
	}
}

func TestBridge_HandleMeshcorePacket_IgnoresNonGrpTxtPackets(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)

	pkt, err := meshcore.BuildPacket(meshcore.Packet{Route: meshcore.RouteFlood, PayloadType: meshcore.PayloadTypeAdvert, Payload: []byte{1, 2, 3}})
	if err != nil {
		t.Fatalf("BuildPacket: %v", err)
	}
	parsed, _ := meshcore.ParsePacket(pkt)
	b.handleMeshcorePacket(meshcore.LogRxData{Packet: parsed})

	select {
	case p := <-posts:
		t.Fatalf("unexpected post: %+v", p)
	case <-time.After(100 * time.Millisecond):
		// expected: nothing posted
	}
}

func TestBridge_HandleMeshcorePacket_WrongChannelSecretIsIgnored(t *testing.T) {
	mGeneral, posts := newTestMapping(t, "general", "#general")
	b := newTestBridge(mGeneral)

	otherSecret := meshcore.HashtagChannelSecret("#other")
	lrx := buildLogRxData(t, otherSecret, 1000, "Alice: hi")
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-posts:
		t.Fatalf("unexpected post for a message on a different channel: %+v", p)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBridge_HandleMeshcorePacket_IgnoresDisabledMapping(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	m.meshcoreEnabled = false
	b := newTestBridge(m)

	lrx := buildLogRxData(t, m.secret, 1000, "Alice: hi")
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-posts:
		t.Fatalf("unexpected post for a meshcore-disabled mapping: %+v", p)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBridge_HandleMeshcorePacket_SuppressesDuplicateFloodedPacket(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)

	lrx := buildLogRxData(t, m.secret, 2000, "Alice: hi again")
	b.handleMeshcorePacket(lrx)
	b.handleMeshcorePacket(lrx) // simulates the same flood arriving via a second relay hop

	select {
	case <-posts:
	case <-time.After(2 * time.Second):
		t.Fatal("expected exactly one post")
	}
	select {
	case p := <-posts:
		t.Fatalf("expected the duplicate to be suppressed, got a second post: %+v", p)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBridge_HandleMeshcorePacket_SuppressesOwnEcho(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)

	b.markOutboundSent(meshcoreEchoKey(m.channelHash, "Bob: from discord"))
	lrx := buildLogRxData(t, m.secret, 3000, "Bob: from discord")
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-posts:
		t.Fatalf("expected our own echoed send to be suppressed, got post: %+v", p)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBridge_ConsumeSelfEcho_OnlyConsumesOnce(t *testing.T) {
	b := newTestBridge()
	key := meshcoreEchoKey(7, "hi")
	b.markOutboundSent(key)
	if !b.consumeSelfEcho(key) {
		t.Fatal("expected first consume to report a match")
	}
	if b.consumeSelfEcho(key) {
		t.Fatal("expected second consume to report no match (already consumed)")
	}
}

func TestBridge_Sweep_ExpiresOldEntries(t *testing.T) {
	b := newTestBridge()
	b.mu.Lock()
	b.recentInbound["stale"] = time.Now().Add(-2 * inboundDedupTTL)
	b.recentInbound["fresh"] = time.Now()
	b.recentOutbound["stale"] = time.Now().Add(-2 * selfEchoTTL)
	b.recentOutbound["fresh"] = time.Now()
	b.mu.Unlock()

	b.sweep()

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.recentInbound["stale"]; ok {
		t.Error("stale inbound entry should have been swept")
	}
	if _, ok := b.recentInbound["fresh"]; !ok {
		t.Error("fresh inbound entry should survive sweep")
	}
	if _, ok := b.recentOutbound["stale"]; ok {
		t.Error("stale outbound entry should have been swept")
	}
	if _, ok := b.recentOutbound["fresh"]; !ok {
		t.Error("fresh outbound entry should survive sweep")
	}
}
