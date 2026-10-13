package bridge

import (
	"testing"
	"time"

	generated "github.com/meshnet-gophers/meshtastic-go/meshtastic"

	"github.com/hectospark/hoplink/internal/meshcore"
)

// Regression tests: our own outbound packet, flooded back to us by the mesh,
// must never be reposted to Discord / relayed as new content — not even once
// retry_on_no_repeat's pending-echo accounting (recentOutbound) has expired
// or been consumed. See markOwnPacket.

func assertNoPost(t *testing.T, posts chan webhookPost) {
	t.Helper()
	select {
	case p := <-posts:
		t.Fatalf("expected our own echo to be suppressed, got post: %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

// sendOneMeshcoreChunk transmits text via the real outbound path and returns
// the payload timestamp it was stamped with.
func sendOneMeshcoreChunk(t *testing.T, b *Bridge, m *mapping, sent chan []byte, text string) uint32 {
	t.Helper()
	b.transmitMeshcore(m, text, 1_700_000_000, 1, 1, 0)
	select {
	case raw := <-sent:
		pkt, err := meshcore.ParsePacket(raw)
		if err != nil {
			t.Fatalf("ParsePacket: %v", err)
		}
		dec, ok := meshcore.DecryptGroupText(m.secret, pkt.Payload)
		if !ok {
			t.Fatal("DecryptGroupText failed on our own packet")
		}
		return dec.TimestampUnix
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the send")
	}
	return 0
}

func TestBridge_OwnMeshcoreEcho_SuppressedAfterSelfEchoEntryExpires(t *testing.T) {
	session, sent := dialTestSession(t)
	m, posts := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.hashSize = 3

	const text = "Alice: hello"
	ts := sendOneMeshcoreChunk(t, b, m, sent, text)

	// Age the pending self-echo entry past selfEchoTTL and sweep it, as the
	// housekeeping loop would on a slow multi-hop echo.
	key := meshcoreEchoKey(m.channelHash, text)
	b.mu.Lock()
	p := b.recentOutbound[key]
	p.sentAt = time.Now().Add(-2 * selfEchoTTL)
	b.recentOutbound[key] = p
	b.mu.Unlock()
	b.sweep()

	b.deliverMeshcoreChannelText("raw log", m.channelHash, ts, text, nil)
	b.deliverMeshcoreChannelText("sync", m.channelHash, ts, text, nil)
	assertNoPost(t, posts)
}

func TestBridge_OwnMeshcoreEcho_SuppressedInRetryGap(t *testing.T) {
	session, sent := dialTestSession(t)
	m, posts := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.hashSize = 3

	const text = "Alice: hello"
	ts := sendOneMeshcoreChunk(t, b, m, sent, text)

	// The retry timer has consumed the pending entry and decided to
	// retransmit, but the retransmit hasn't re-marked it yet (e.g. it's still
	// in busy_wait) when the original's late echo arrives.
	if !b.echoUnheard(meshcoreEchoKey(m.channelHash, text)) {
		t.Fatal("expected the send to still be pending")
	}
	b.deliverMeshcoreChannelText("raw log", m.channelHash, ts, text, nil)
	assertNoPost(t, posts)
}

func TestBridge_OwnMeshcoreEcho_StillCountsAsRepeatForRetry(t *testing.T) {
	session, sent := dialTestSession(t)
	m, posts := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.hashSize = 3

	const text = "Alice: hello"
	ts := sendOneMeshcoreChunk(t, b, m, sent, text)

	// Seeding the dedup set must not short-circuit the self-echo accounting:
	// a timely echo still consumes the pending entry, so no retry fires.
	b.deliverMeshcoreChannelText("raw log", m.channelHash, ts, text, nil)
	if b.echoUnheard(meshcoreEchoKey(m.channelHash, text)) {
		t.Error("echo should have been counted as a heard repeat")
	}
	assertNoPost(t, posts)
}

func TestBridge_MeshcoreMessageFromOthers_SameTextDifferentTimestampStillPosts(t *testing.T) {
	session, sent := dialTestSession(t)
	m, posts := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.hashSize = 3

	ts := sendOneMeshcoreChunk(t, b, m, sent, "Alice: hello")
	b.unmarkOutboundSent(meshcoreEchoKey(m.channelHash, "Alice: hello"))

	// A genuinely different packet (another sender's timestamp) is not ours.
	b.deliverMeshcoreChannelText("raw log", m.channelHash, ts+5, "Alice: hello", nil)
	select {
	case <-posts:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a different packet with the same text to be posted")
	}
}

func TestBridge_OwnMeshtasticEcho_SuppressedAfterSelfEchoEntryExpires(t *testing.T) {
	session, _, radio := dialTestMeshtasticSession(t)
	m, posts := newTestMeshtasticMapping(t, "general", "chan-1", "general")
	b := newTestBridge(m)
	b.SetMeshtasticSession(session)

	const text = "Alice: from discord"
	const pktID = 0x1234
	b.transmitMeshtastic(m, text, pktID, 1, 1, 0)
	b.unmarkOutboundSent(meshtasticEchoKey(1, text)) // pending entry gone

	_ = radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From: 1, Channel: 1, Id: pktID, RxSnr: 8.5, RxRssi: -60,
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_TEXT_MESSAGE_APP,
			Payload: []byte(text),
		}},
	}}})
	pumpOneTextMessage(t, b, session)
	assertNoPost(t, posts)
}
