package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/hectospark/hoplink/internal/meshcore"
)

func TestBridge_HandleMeshcorePacket_LogsReceivedViaRawLogWithDebug(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	b.debug = true
	buf := captureLog(t)

	lrx := buildLogRxData(t, m.secret, 1000, "Alice: hi")
	b.handleMeshcorePacket(lrx)
	<-posts

	if !strings.Contains(buf.String(), "meshcore: received via raw log on channel_hash") {
		t.Errorf("expected a raw-log receipt debug line, got: %s", buf.String())
	}
}

func TestBridge_HandleMeshcoreChannelMessage_LogsReceivedViaSyncWithDebug(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	b.debug = true
	b.hashBySlot = map[byte]byte{5: m.channelHash}
	buf := captureLog(t)

	b.handleMeshcoreChannelMessage(meshcore.ChannelMessage{
		SlotIndex:     5,
		TimestampUnix: 1000,
		Text:          "Alice: hi via sync",
	})
	<-posts

	if !strings.Contains(buf.String(), "meshcore: received via sync on channel_hash") {
		t.Errorf("expected a sync receipt debug line, got: %s", buf.String())
	}
}

func TestBridge_HandleMeshcoreChannelMessage_PostsToDiscord(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	b.hashBySlot = map[byte]byte{5: m.channelHash}

	b.handleMeshcoreChannelMessage(meshcore.ChannelMessage{
		SlotIndex:     5,
		TimestampUnix: 1000,
		Text:          "Alice: hi via sync",
	})

	select {
	case p := <-posts:
		if p.username != "Alice" || p.content != "hi via sync" {
			t.Errorf("got post %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook post")
	}
}

func TestBridge_HandleMeshcoreChannelMessage_IgnoresUnregisteredSlot(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	b.hashBySlot = map[byte]byte{5: m.channelHash} // message arrives on a different, unmapped slot

	b.handleMeshcoreChannelMessage(meshcore.ChannelMessage{
		SlotIndex:     6,
		TimestampUnix: 1000,
		Text:          "Alice: hi",
	})

	select {
	case p := <-posts:
		t.Fatalf("expected no post for an unrecognised slot, got %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestBridge_DualPath_SyncAndRawLogDedupeTheSameMessage(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	b.hashBySlot = map[byte]byte{5: m.channelHash}

	// Raw-log path delivers first.
	lrx := buildLogRxData(t, m.secret, 1000, "Alice: hi")
	b.handleMeshcorePacket(lrx)
	select {
	case <-posts:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the raw-log delivery")
	}

	// The sync path then reports the *same* message (same channel,
	// timestamp, and text) — as it would if both mechanisms independently
	// caught the same physical packet. It must be deduped, not re-posted.
	b.handleMeshcoreChannelMessage(meshcore.ChannelMessage{
		SlotIndex:     5,
		TimestampUnix: 1000,
		Text:          "Alice: hi",
	})

	select {
	case p := <-posts:
		t.Fatalf("expected the sync path's delivery of the same message to be deduped, got a second post: %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestBridge_DualPath_SyncOnlyDeliversWhenRawLogMisses(t *testing.T) {
	// Symmetric to the above: if only the sync path ever sees a message
	// (simulating the raw-log push having been dropped), it must still
	// reach Discord — that's the whole point of having two paths.
	m, posts := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)
	b.hashBySlot = map[byte]byte{5: m.channelHash}

	b.handleMeshcoreChannelMessage(meshcore.ChannelMessage{
		SlotIndex:     5,
		TimestampUnix: 2000,
		Text:          "Bob: only via sync",
	})

	select {
	case p := <-posts:
		if p.username != "Bob" || p.content != "only via sync" {
			t.Errorf("got post %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the sync-only delivery")
	}
}

func TestBridge_RegisterMeshcoreChannels_PopulatesHashBySlot(t *testing.T) {
	m, _ := newTestMapping(t, "general", "#general")
	b := newTestBridge(m)

	addr, _ := startTinyFakeRadio(t, func(cmd byte, frame []byte) []byte {
		switch cmd {
		case meshcore.CmdAppStart:
			return selfInfoFrame("fake-radio")
		case meshcore.CmdGetChannel:
			// Every slot reports empty; RegisterChannel should claim slot 1.
			resp := make([]byte, 2+32+16)
			resp[0] = meshcore.FrameChannelInfo
			resp[1] = frame[1]
			return resp
		case meshcore.CmdSetChannel:
			return []byte{meshcore.FrameOK}
		}
		return nil
	})
	session, _, err := meshcore.Dial(addr, "hoplink-test")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	b.registerMeshcoreChannels(session)

	hash, ok := b.hashBySlot[1]
	if !ok {
		t.Fatalf("expected slot 1 to be registered, got hashBySlot=%v", b.hashBySlot)
	}
	if hash != m.channelHash {
		t.Errorf("hashBySlot[1] = %#x, want %#x", hash, m.channelHash)
	}
}

func TestBridge_RegisterMeshcoreChannels_SkipsBridgesWithMeshcoreDisabled(t *testing.T) {
	m, _ := newTestMeshtasticMapping(t, "meshtastic-only", "chan-1", "general") // no meshcore at all
	b := newTestBridge(m)

	addr, _ := startTinyFakeRadio(t, func(cmd byte, frame []byte) []byte {
		if cmd == meshcore.CmdAppStart {
			return selfInfoFrame("fake-radio")
		}
		t.Errorf("unexpected command %#x sent when no bridge has meshcore enabled", cmd)
		return nil
	})
	session, _, err := meshcore.Dial(addr, "hoplink-test")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	b.registerMeshcoreChannels(session)

	if len(b.hashBySlot) != 0 {
		t.Errorf("expected no channel registrations, got %v", b.hashBySlot)
	}
}
