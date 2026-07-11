package bridge

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hectospark/hoplink/internal/config"
	"github.com/hectospark/hoplink/internal/discord"
	"github.com/hectospark/hoplink/internal/meshcore"
)

// dialTestSession starts a tinyFakeRadio and dials a real *meshcore.Session
// against it, capturing every CMD_SEND_RAW_PACKET frame it receives.
func dialTestSession(t *testing.T) (*meshcore.Session, chan []byte) {
	t.Helper()
	sentPackets := make(chan []byte, 8)
	addr, _ := startTinyFakeRadio(t, func(cmd byte, frame []byte) []byte {
		switch cmd {
		case meshcore.CmdAppStart:
			return selfInfoFrame("fake-radio")
		case meshcore.CmdSendRawPacket:
			sentPackets <- append([]byte(nil), frame[2:]...) // strip [cmd][priority]
			return []byte{meshcore.FrameOK}
		}
		return nil
	})

	session, _, err := meshcore.Dial(addr, "hoplink-test")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, sentPackets
}

func TestBridge_HandleDiscordMessage_SendsComposedTextToMesh(t *testing.T) {
	session, sentPackets := dialTestSession(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	secret := meshcore.HashtagChannelSecret("#general")
	chHash, _ := meshcore.ChannelHash(secret)
	m := &mapping{
		cfg:             config.Bridge{Name: "general", DiscordChannelID: "chan-1"},
		secret:          secret,
		channelHash:     chHash,
		webhook:         discord.NewWebhookSender(server.URL + "/api/webhooks/1/tok"),
		maxBytes:        320,
		meshcoreEnabled: true,
	}
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.route = meshcore.RouteFlood
	b.hashSize = 3

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  "chan-1",
		AuthorName: "Alice",
		Content:    "hello from discord",
	})

	select {
	case raw := <-sentPackets:
		pkt, err := meshcore.ParsePacket(raw)
		if err != nil {
			t.Fatalf("ParsePacket: %v", err)
		}
		if pkt.PayloadType != meshcore.PayloadTypeGrpTxt {
			t.Fatalf("PayloadType = %v, want GrpTxt", pkt.PayloadType)
		}
		if pkt.HashSize != 3 {
			t.Errorf("HashSize = %d, want 3 (never 1-byte path hashes)", pkt.HashSize)
		}
		dec, ok := meshcore.DecryptGroupText(secret, pkt.Payload)
		if !ok {
			t.Fatal("DecryptGroupText failed on the packet the bridge sent")
		}
		if dec.Text != "Alice: hello from discord" {
			t.Errorf("text = %q, want %q", dec.Text, "Alice: hello from discord")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the bridge to send a raw packet")
	}

	if !b.consumeSelfEcho(meshcoreEchoKey(chHash, "Alice: hello from discord")) {
		t.Error("expected handleDiscordMessage to have recorded a self-echo entry")
	}
}

func TestBridge_HandleDiscordMessage_AppliesConfiguredFloodScope(t *testing.T) {
	session, sentPackets := dialTestSession(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	secret := meshcore.HashtagChannelSecret("#general")
	chHash, _ := meshcore.ChannelHash(secret)
	m := &mapping{
		cfg:             config.Bridge{Name: "general", DiscordChannelID: "chan-1"},
		secret:          secret,
		channelHash:     chHash,
		webhook:         discord.NewWebhookSender(server.URL + "/api/webhooks/1/tok"),
		maxBytes:        320,
		meshcoreEnabled: true,
	}
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.route = meshcore.RouteFlood
	b.hashSize = 3
	b.scopeKey = meshcore.FloodScopeKey("myregion")

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  "chan-1",
		AuthorName: "Alice",
		Content:    "scoped hello",
	})

	select {
	case raw := <-sentPackets:
		pkt, err := meshcore.ParsePacket(raw)
		if err != nil {
			t.Fatalf("ParsePacket: %v", err)
		}
		if pkt.Route != meshcore.RouteTransportFlood {
			t.Errorf("Route = %v, want RouteTransportFlood when a scope is configured", pkt.Route)
		}
		wantCode, err := meshcore.CalcTransportCode(b.scopeKey, meshcore.PayloadTypeGrpTxt, pkt.Payload)
		if err != nil {
			t.Fatalf("CalcTransportCode: %v", err)
		}
		gotCode := uint16(pkt.TransportCodes[0]) | uint16(pkt.TransportCodes[1])<<8
		if gotCode != wantCode {
			t.Errorf("transport_codes[0] = %#x, want %#x", gotCode, wantCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the bridge to send a raw packet")
	}
}

func TestBridge_HandleDiscordMessage_IgnoresUnmappedChannel(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.route = meshcore.RouteFlood

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  "some-other-channel",
		AuthorName: "Alice",
		Content:    "hello",
	})

	select {
	case raw := <-sentPackets:
		t.Fatalf("unexpected send for an unmapped channel: %v", raw)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBridge_HandleDiscordMessage_IgnoresOwnWebhookLoop(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.route = meshcore.RouteFlood

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  m.cfg.DiscordChannelID,
		AuthorName: "SomeNode",
		Content:    "relayed by our own webhook",
		WebhookID:  m.webhook.ID(),
	})

	select {
	case raw := <-sentPackets:
		t.Fatalf("unexpected send for our own webhook's echoed message: %v", raw)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBridge_HandleDiscordMessage_IgnoresEmptyContent(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.route = meshcore.RouteFlood

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  m.cfg.DiscordChannelID,
		AuthorName: "Alice",
		Content:    "   ",
	})

	select {
	case raw := <-sentPackets:
		t.Fatalf("unexpected send for blank content: %v", raw)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBridge_HandleDiscordMessage_SkipsSendWhenMeshcoreSessionNotAttached(t *testing.T) {
	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m) // no session attached at all

	// Should log and return, not panic or hang.
	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  m.cfg.DiscordChannelID,
		AuthorName: "Alice",
		Content:    "hello",
	})
}

func TestBridge_HandleDiscordMessage_IgnoresMismatchedGuild(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	m.cfg.GuildID = "guild-a"
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.route = meshcore.RouteFlood

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  m.cfg.DiscordChannelID,
		AuthorName: "Alice",
		Content:    "hello",
		GuildID:    "guild-b",
	})

	select {
	case raw := <-sentPackets:
		t.Fatalf("unexpected send for a message from an unconfigured guild: %v", raw)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBridge_HandleDiscordMessage_AllowsMatchingGuild(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	m.cfg.GuildID = "guild-a"
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.route = meshcore.RouteFlood

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  m.cfg.DiscordChannelID,
		AuthorName: "Alice",
		Content:    "hello",
		GuildID:    "guild-a",
	})

	select {
	case <-sentPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a send for a message from the configured guild")
	}
}
