package bridge

import (
	"errors"
	"testing"
	"time"

	"github.com/hectospark/hoplink/internal/discord"
)

// fakeNotifier substitutes for *discord.Bot in tests, capturing Reply calls
// without making a real Discord API call.
type fakeNotifier struct {
	replies chan replyCall
	err     error
}

type replyCall struct {
	channelID, messageID, content string
}

func newFakeNotifier() *fakeNotifier {
	return &fakeNotifier{replies: make(chan replyCall, 4)}
}

func (f *fakeNotifier) Reply(channelID, messageID, content string) error {
	if f.err != nil {
		return f.err
	}
	f.replies <- replyCall{channelID: channelID, messageID: messageID, content: content}
	return nil
}

func TestComposedLength(t *testing.T) {
	if got := composedLength("Alice", "hi"); got != len("Alice: hi") {
		t.Errorf("composedLength = %d, want %d", got, len("Alice: hi"))
	}
}

func TestBridge_HandleDiscordMessage_RejectsOversizedMessageAndNotifies(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	m.maxBytes = 20 // small limit to trigger easily
	notify := newFakeNotifier()
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.notify = notify

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  m.cfg.DiscordChannelID,
		MessageID:  "msg-1",
		AuthorName: "Alice",
		Content:    "this message is definitely longer than twenty bytes",
	})

	select {
	case raw := <-sentPackets:
		t.Fatalf("expected no mesh send for an oversized message, got: %v", raw)
	case <-time.After(200 * time.Millisecond):
	}

	select {
	case r := <-notify.replies:
		if r.channelID != m.cfg.DiscordChannelID || r.messageID != "msg-1" {
			t.Errorf("unexpected reply target: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a too-long notice to be sent")
	}
}

func TestBridge_HandleDiscordMessage_AllowsMessageAtExactLimit(t *testing.T) {
	session, sentPackets := dialTestSession(t)
	m, _ := newTestMapping(t, "general", "#general")
	m.maxBytes = len("Alice: hi")
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  m.cfg.DiscordChannelID,
		AuthorName: "Alice",
		Content:    "hi",
	})

	select {
	case <-sentPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a send for a message exactly at the byte limit")
	}
}

func TestBridge_NotifyTooLong_LogsReplyFailureWithoutPanicking(t *testing.T) {
	notify := &fakeNotifier{err: errors.New("discord unavailable")}
	b := &Bridge{notify: notify}
	// Must not panic even though the notifier itself errors.
	b.notifyTooLong(discord.IncomingMessage{ChannelID: "c", MessageID: "m"}, 500, 320)
}
