package bridge

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hectospark/hoplink/internal/discord"
)

type fakeReactor struct {
	mu     sync.Mutex
	events []string // "+emoji" / "-emoji", in order
	fail   bool
}

func (f *fakeReactor) React(_, messageID, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, messageID+" +"+emoji)
	if f.fail {
		return fmt.Errorf("403 Missing Permissions")
	}
	return nil
}

func (f *fakeReactor) Unreact(_, messageID, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, messageID+" -"+emoji)
	return nil
}

func (f *fakeReactor) waitFor(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		got := strings.Join(f.events, " | ")
		f.mu.Unlock()
		if got == strings.Join(want, " | ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reactions = [%s], want [%s]", got, strings.Join(want, " | "))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func discordMsg(id, content string) discord.IncomingMessage {
	return discord.IncomingMessage{ChannelID: "chan-1", MessageID: id, AuthorName: "Alice", Content: content}
}

func TestReceipt_DeliveredMessageGetsTick(t *testing.T) {
	b, _, sent, _ := newDeliveryTestBridge(t)
	fr := &fakeReactor{}
	b.react = fr

	b.handleDiscordMessage(discordMsg("m1", "hello"))
	raw := <-sent
	b.handleMeshcorePacket(relayedCopy(t, raw, "a1a1a1"))

	fr.waitFor(t, "m1 +"+reactAccepted, "m1 +"+reactDelivered, "m1 -"+reactAccepted)
}

func TestReceipt_UnconfirmedMessageGetsWarning(t *testing.T) {
	b, _, sent, _ := newDeliveryTestBridge(t)
	fr := &fakeReactor{}
	b.react = fr

	b.handleDiscordMessage(discordMsg("m1", "hello"))
	<-sent
	fr.waitFor(t, "m1 +"+reactAccepted, "m1 +"+reactUnconfirmed, "m1 -"+reactAccepted)
}

func TestReceipt_WaitsForRetryBeforeSettling(t *testing.T) {
	b, _, sent, _ := newDeliveryTestBridge(t)
	b.meshcoreRetryOnNoRepeat = true
	fr := &fakeReactor{}
	b.react = fr

	b.handleDiscordMessage(discordMsg("m1", "hello"))
	<-sent
	retry := <-sent
	b.handleMeshcorePacket(relayedCopy(t, retry, "a1a1a1")) // the retry got through
	fr.waitFor(t, "m1 +"+reactAccepted, "m1 +"+reactDelivered, "m1 -"+reactAccepted)
}

func TestReceipt_MultiChunkNeedsEveryChunkConfirmed(t *testing.T) {
	b, _, sent, _ := newDeliveryTestBridge(t)
	fr := &fakeReactor{}
	b.react = fr

	b.handleDiscordMessage(discordMsg("m1", strings.Repeat("x", meshcoreMaxChunkBytes+20)))
	first := <-sent
	<-sent // second chunk: never heard
	b.handleMeshcorePacket(relayedCopy(t, first, "a1a1a1"))
	fr.waitFor(t, "m1 +"+reactAccepted, "m1 +"+reactUnconfirmed, "m1 -"+reactAccepted)
}

func TestReceipt_NotConnectedGetsWarningOnly(t *testing.T) {
	b, _, _, _ := newDeliveryTestBridge(t)
	b.SetMeshcoreSession(nil)
	fr := &fakeReactor{}
	b.react = fr

	b.handleDiscordMessage(discordMsg("m1", "hello"))
	fr.waitFor(t, "m1 +"+reactUnconfirmed)
}

func TestReceipt_OffByDefault(t *testing.T) {
	b, _, sent, _ := newDeliveryTestBridge(t)
	if b.react != nil {
		t.Fatal("test bridge should have reactions off")
	}
	b.handleDiscordMessage(discordMsg("m1", "hello")) // must not panic on nil receipt
	<-sent
	time.Sleep(200 * time.Millisecond)
}

func TestReceipt_ReactionErrorIsLoggedNotFatal(t *testing.T) {
	buf := captureLog(t)
	b, _, sent, _ := newDeliveryTestBridge(t)
	fr := &fakeReactor{fail: true}
	b.react = fr

	b.handleDiscordMessage(discordMsg("m1", "hello"))
	<-sent
	waitForLog(t, buf, "Add Reactions permission")
}

func TestReceipt_MeshOriginSendsHaveNoReceipt(t *testing.T) {
	b, m, sent, _ := newDeliveryTestBridge(t)
	fr := &fakeReactor{}
	b.react = fr

	b.sendMeshcore(m, "Bob (MT)", "relayed from meshtastic")
	<-sent
	time.Sleep(300 * time.Millisecond)
	fr.waitFor(t) // nothing
}

// The same text sent twice within one second is byte-identical — one packet
// hash, one delivery record — but two Discord messages: both must settle.
func TestReceipt_IdenticalMessagesInSameSecondBothSettle(t *testing.T) {
	b, _, sent, _ := newDeliveryTestBridge(t)
	fr := &fakeReactor{}
	b.react = fr

	b.handleDiscordMessage(discordMsg("m1", "ok"))
	raw := <-sent
	b.handleDiscordMessage(discordMsg("m2", "ok"))
	raw2 := <-sent
	if string(raw) != string(raw2) {
		t.Skip("sends straddled a second boundary; not the same packet")
	}
	b.handleMeshcorePacket(relayedCopy(t, raw, "a1a1a1"))

	deadline := time.Now().Add(3 * time.Second)
	for {
		fr.mu.Lock()
		got := strings.Join(fr.events, " | ")
		fr.mu.Unlock()
		if strings.Contains(got, "m1 +"+reactDelivered) && strings.Contains(got, "m2 +"+reactDelivered) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("both messages should end ✅, reactions = [%s]", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
