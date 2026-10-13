package bridge

import (
	"testing"
	"time"

	generated "github.com/meshnet-gophers/meshtastic-go/meshtastic"
)

func expectOnePost(t *testing.T, posts chan webhookPost) {
	t.Helper()
	select {
	case <-posts:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a post")
	}
}

func TestBridge_ContentDedup_MergesMeshcoreResendWithinWindow(t *testing.T) {
	m, posts := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)
	b.contentDedupWindow = time.Minute

	// A "resend" is a new packet: same text, new timestamp.
	b.deliverMeshcoreChannelText("raw log", m.channelHash, 1000, "Bob: anyone about?", nil)
	expectOnePost(t, posts)
	b.deliverMeshcoreChannelText("raw log", m.channelHash, 1020, "Bob: anyone about?", nil)
	assertNoPost(t, posts)
}

func TestBridge_ContentDedup_DisabledByDefault(t *testing.T) {
	m, posts := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)

	b.deliverMeshcoreChannelText("raw log", m.channelHash, 1000, "Bob: ok", nil)
	expectOnePost(t, posts)
	b.deliverMeshcoreChannelText("raw log", m.channelHash, 1020, "Bob: ok", nil)
	expectOnePost(t, posts)
}

func TestBridge_ContentDedup_PostsAgainOnceWindowElapses(t *testing.T) {
	m, posts := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)
	b.contentDedupWindow = time.Minute

	b.deliverMeshcoreChannelText("raw log", m.channelHash, 1000, "Bob: ok", nil)
	expectOnePost(t, posts)
	b.mu.Lock()
	b.recentContent[meshcoreContentKey(m.channelHash, "Bob: ok")] = time.Now().Add(-2 * time.Minute)
	b.mu.Unlock()
	b.deliverMeshcoreChannelText("raw log", m.channelHash, 1100, "Bob: ok", nil)
	expectOnePost(t, posts)
}

func TestBridge_ContentDedup_DifferentSenderNotMerged(t *testing.T) {
	m, posts := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)
	b.contentDedupWindow = time.Minute

	b.deliverMeshcoreChannelText("raw log", m.channelHash, 1000, "Bob: ok", nil)
	expectOnePost(t, posts)
	b.deliverMeshcoreChannelText("raw log", m.channelHash, 1001, "Carol: ok", nil)
	expectOnePost(t, posts)
}

func TestBridge_ContentDedup_SweepForgetsExpiredEntries(t *testing.T) {
	b := newTestBridge()
	b.contentDedupWindow = time.Minute
	b.isRepeatedContent("k")
	b.mu.Lock()
	b.recentContent["k"] = time.Now().Add(-2 * time.Minute)
	b.mu.Unlock()
	b.sweep()
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.recentContent["k"]; ok {
		t.Error("expected sweep to drop an entry older than the window")
	}
}

func TestBridge_ContentDedup_MergesMeshtasticResendWithinWindow(t *testing.T) {
	session, _, radio := dialTestMeshtasticSession(t)
	m, posts := newTestMeshtasticMapping(t, "general", "chan-1", "general")
	b := newTestBridge(m)
	b.SetMeshtasticSession(session)
	b.contentDedupWindow = time.Minute

	for _, id := range []uint32{101, 102} { // a resend gets a fresh packet ID
		_ = radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
			From: 7, Channel: 1, Id: id,
			PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
				Portnum: generated.PortNum_TEXT_MESSAGE_APP,
				Payload: []byte("anyone about?"),
			}},
		}}})
		pumpOneTextMessage(t, b, session)
	}
	expectOnePost(t, posts)
	assertNoPost(t, posts)
}
