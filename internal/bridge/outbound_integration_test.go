package bridge

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
		webhook:         discord.NewWebhookSender(server.URL+"/api/webhooks/1/tok", "test"),
		discordEnabled:  true,
		maxBytes:        320,
		meshcoreEnabled: true,
	}
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
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

	if b.consumeSelfEcho(meshcoreEchoKey(chHash, "Alice: hello from discord"), nil) == notSelfEcho {
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
		scopeKey:        meshcore.FloodScopeKey("myregion"),
		webhook:         discord.NewWebhookSender(server.URL+"/api/webhooks/1/tok", "test"),
		discordEnabled:  true,
		maxBytes:        320,
		meshcoreEnabled: true,
	}
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.hashSize = 3

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
		wantCode, err := meshcore.CalcTransportCode(m.scopeKey, meshcore.PayloadTypeGrpTxt, pkt.Payload)
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

// withShortRepeatRetryWait sets b's meshcore/meshtastic retry_wait_ms fields
// directly to d, so retry_on_no_repeat tests don't have to sleep for the
// real production wait. b is a fresh instance per test, so no restore is
// needed.
func withShortRepeatRetryWait(b *Bridge, d time.Duration) {
	b.meshcoreRetryWait = d
	b.meshtasticRetryWait = d
}

func TestBridge_TransmitMeshcore_RetriesWhenNoRepeatHeard(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	withShortRepeatRetryWait(b, 50*time.Millisecond)
	b.SetMeshcoreSession(session)
	b.meshcoreRetryOnNoRepeat = true

	b.transmitMeshcore(m, "Alice: hello", uint32(time.Now().Unix()), 1, 1, 0)

	for i := 0; i < 2; i++ {
		select {
		case <-sentPackets:
		case <-time.After(2 * time.Second):
			t.Fatalf("expected 2 sends (original + one retry), only got %d", i)
		}
	}

	select {
	case raw := <-sentPackets:
		t.Fatalf("expected exactly one retry, got a further send: %v", raw)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestBridge_TransmitMeshcore_NoRetryWhenRepeatHeard(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	withShortRepeatRetryWait(b, 200*time.Millisecond)
	b.SetMeshcoreSession(session)
	b.meshcoreRetryOnNoRepeat = true

	b.transmitMeshcore(m, "Alice: hello", uint32(time.Now().Unix()), 1, 1, 0)

	var raw []byte
	select {
	case raw = <-sentPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the original send")
	}

	// The mesh repeats our own packet back to us via a repeater.
	b.handleMeshcorePacket(relayedCopy(t, raw, "112233"))

	select {
	case raw := <-sentPackets:
		t.Fatalf("expected no retry once a repeat was heard, got a send: %v", raw)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestBridge_TransmitMeshcore_NoRetryWhenDisabled(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	withShortRepeatRetryWait(b, 50*time.Millisecond)
	b.SetMeshcoreSession(session)
	// b.meshcoreRetryOnNoRepeat left false: the default.

	b.transmitMeshcore(m, "Alice: hello", uint32(time.Now().Unix()), 1, 1, 0)

	select {
	case <-sentPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the original send")
	}

	select {
	case raw := <-sentPackets:
		t.Fatalf("expected no retry with retry_on_no_repeat disabled, got a send: %v", raw)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestBridge_TransmitMeshcore_RetriesWhenOnlyRepeatHeardIsFromIgnoredRepeater
// verifies meshcore.ignore_repeat_from: hearing our own message repeated
// back via a repeater on the ignore list must NOT cancel the pending retry
// — it should count exactly as if no repeat had been heard at all.
func TestBridge_TransmitMeshcore_RetriesWhenOnlyRepeatHeardIsFromIgnoredRepeater(t *testing.T) {
	ignoredHash := []byte{0xaa, 0xbb, 0xcc}
	m, _ := newTestMapping(t, "general", "#general")
	m.ignoreRepeatFrom = [][]byte{ignoredHash}
	session, sentPackets := dialTestSession(t)
	b := newTestBridge(m)
	withShortRepeatRetryWait(b, 50*time.Millisecond)
	b.SetMeshcoreSession(session)
	b.meshcoreRetryOnNoRepeat = true

	b.transmitMeshcore(m, "Alice: hello", uint32(time.Now().Unix()), 1, 1, 0)

	var raw []byte
	select {
	case raw = <-sentPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the original send")
	}

	// The mesh repeats our own packet back to us, but only via the ignored
	// repeater — should not count as "heard" for retry purposes.
	b.handleMeshcorePacket(relayedCopy(t, raw, hex.EncodeToString(ignoredHash)))

	select {
	case <-sentPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a retransmit: the only repeat heard was from an ignored repeater")
	}
}

// TestBridge_TransmitMeshcore_NoRetryWhenRepeatHeardFromNonIgnoredRepeater is
// the counterpart: a repeat heard via a repeater NOT on the ignore list
// counts normally and cancels the pending retry, even with an ignore list
// configured (for a different repeater).
func TestBridge_TransmitMeshcore_NoRetryWhenRepeatHeardFromNonIgnoredRepeater(t *testing.T) {
	ignoredHash := []byte{0xaa, 0xbb, 0xcc}
	otherHash := []byte{0x11, 0x22, 0x33}
	m, _ := newTestMapping(t, "general", "#general")
	m.ignoreRepeatFrom = [][]byte{ignoredHash}
	session, sentPackets := dialTestSession(t)
	b := newTestBridge(m)
	withShortRepeatRetryWait(b, 200*time.Millisecond)
	b.SetMeshcoreSession(session)
	b.meshcoreRetryOnNoRepeat = true

	b.transmitMeshcore(m, "Alice: hello", uint32(time.Now().Unix()), 1, 1, 0)

	var raw []byte
	select {
	case raw = <-sentPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the original send")
	}

	b.handleMeshcorePacket(relayedCopy(t, raw, hex.EncodeToString(otherHash)))

	select {
	case raw := <-sentPackets:
		t.Fatalf("expected no retry — the repeat was heard from a non-ignored repeater, got a send: %v", raw)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestBridge_TransmitMeshcore_RetransmitIsByteIdentical(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	withShortRepeatRetryWait(b, 50*time.Millisecond)
	b.SetMeshcoreSession(session)
	b.meshcoreRetryOnNoRepeat = true

	b.transmitMeshcore(m, "Alice: hello", uint32(time.Now().Unix()), 1, 1, 0)

	var frames [][]byte
	for i := 0; i < 2; i++ {
		select {
		case raw := <-sentPackets:
			frames = append(frames, raw)
		case <-time.After(2 * time.Second):
			t.Fatalf("expected 2 sends (original + one retry), only got %d", i)
		}
	}
	// Byte-identical retransmit: mesh-side dedup (and sibling bridges'
	// timestamp-keyed dedup) can then recognise it as the same message.
	if !bytes.Equal(frames[0], frames[1]) {
		t.Errorf("retransmit differs from the original send:\n first: %x\nsecond: %x", frames[0], frames[1])
	}
}

func TestBridge_TransmitMeshcore_SendErrorClearsPendingEcho(t *testing.T) {
	// A radio that rejects every raw-packet send.
	addr, _ := startTinyFakeRadio(t, func(cmd byte, frame []byte) []byte {
		switch cmd {
		case meshcore.CmdAppStart:
			return selfInfoFrame("fake-radio")
		case meshcore.CmdSendRawPacket:
			return []byte{meshcore.FrameErr}
		}
		return nil
	})
	session, _, err := meshcore.Dial(addr, "hoplink-test")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)

	b.transmitMeshcore(m, "Alice: hello", uint32(time.Now().Unix()), 1, 1, 0)

	// The send failed, so the pending-echo entry must have been rolled back:
	// otherwise another node genuinely sending this same text within the TTL
	// would be swallowed as "our own echo" and never relayed.
	if b.consumeSelfEcho(meshcoreEchoKey(m.channelHash, "Alice: hello"), nil) != notSelfEcho {
		t.Error("pending echo entry leaked after a failed send")
	}
}

func TestBridge_MarkOutbound_IdenticalMessagesEachTrackOwnEcho(t *testing.T) {
	b := newTestBridge()
	key := meshcoreEchoKey(0xc6, "Alice: hello")
	b.markOutboundSent(key)
	b.markOutboundSent(key)

	if got := b.consumeSelfEcho(key, nil); got != selfEchoHeard {
		t.Fatalf("first echo: got %v, want selfEchoHeard", got)
	}
	if got := b.consumeSelfEcho(key, nil); got != selfEchoHeard {
		t.Fatalf("second identical send's echo was not tracked separately: got %v", got)
	}
	if got := b.consumeSelfEcho(key, nil); got != notSelfEcho {
		t.Fatalf("third delivery should be treated as new content, got %v", got)
	}
}

func TestBridge_TransmitMeshcore_SyncEchoDoesNotCancelIgnoredRepeaterRetry(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	m.ignoreRepeatFrom = [][]byte{{0xaa, 0xbb, 0xcc}}
	b := newTestBridge(m)
	withShortRepeatRetryWait(b, 200*time.Millisecond)
	b.SetMeshcoreSession(session)
	b.meshcoreRetryOnNoRepeat = true

	b.transmitMeshcore(m, "Alice: hello", uint32(time.Now().Unix()), 1, 1, 0)

	select {
	case <-sentPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the original send")
	}

	// The echo arrives via the device-sync path (nil pkt: no hop metadata).
	// With an ignore list configured, a delivery that can't prove which
	// repeater relayed it must not cancel the retry the list exists to force.
	b.deliverMeshcoreChannelText("sync", m.channelHash, 1000, "Alice: hello", nil)

	select {
	case <-sentPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a retransmit: the only echo came via the sync path, which names no repeater")
	}
}

func TestBridge_CloseCancelsRetryTimers(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	withShortRepeatRetryWait(b, 150*time.Millisecond)
	b.SetMeshcoreSession(session)
	b.meshcoreRetryOnNoRepeat = true

	b.transmitMeshcore(m, "Alice: hello", uint32(time.Now().Unix()), 1, 1, 0)

	select {
	case <-sentPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the original send")
	}

	b.Close()

	select {
	case raw := <-sentPackets:
		t.Fatalf("retry fired after Close: %v", raw)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestBridge_SendMeshcore_LogsEachTransmit(t *testing.T) {
	buf := captureLog(t)
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)

	content := strings.Repeat("x", meshcoreMaxChunkBytes*2)
	wantChunks := len(composeChunks("Alice", content, meshcoreMaxChunkBytes))
	if wantChunks < 2 {
		t.Fatalf("test content must split into at least 2 chunks, got %d", wantChunks)
	}

	b.sendMeshcore(m, "Alice", content)

	for i := 0; i < wantChunks; i++ {
		select {
		case <-sentPackets:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for chunk %d/%d", i+1, wantChunks)
		}
	}

	logs := buf.String()
	for _, want := range []string{
		fmt.Sprintf("chunk 1/%d", wantChunks),
		fmt.Sprintf("chunk %d/%d", wantChunks, wantChunks),
		"attempt 1/2",
		"radio accepted",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("expected transmit log to contain %q, logs were:\n%s", want, logs)
		}
	}
}

func TestBridge_SendMeshcore_WaitsChunkDelayBetweenChunks(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.meshcoreChunkDelay = 150 * time.Millisecond

	content := strings.Repeat("x", meshcoreMaxChunkBytes*2)
	wantChunks := len(composeChunks("Alice", content, meshcoreMaxChunkBytes))
	if wantChunks < 2 {
		t.Fatalf("test content must split into at least 2 chunks, got %d", wantChunks)
	}

	start := time.Now()
	b.sendMeshcore(m, "Alice", content)

	for i := 0; i < wantChunks; i++ {
		select {
		case <-sentPackets:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for chunk %d/%d", i+1, wantChunks)
		}
	}

	elapsed := time.Since(start)
	minWait := time.Duration(wantChunks-1) * b.meshcoreChunkDelay
	if elapsed < minWait {
		t.Errorf("elapsed %s sending %d chunks, want at least %s (a delay between each)", elapsed, wantChunks, minWait)
	}
}
