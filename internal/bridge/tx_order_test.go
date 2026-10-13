package bridge

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hectospark/hoplink/internal/discord"
	"github.com/hectospark/hoplink/internal/meshcore"
)

// discordgo runs each event handler on its own goroutine: two Discord
// messages posted close together must still go out one after the other,
// never with their chunks interleaved.
func TestBridge_ConcurrentDiscordMessagesNeverInterleaveChunks(t *testing.T) {
	session, sent := dialTestSession(t)
	m, _ := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.hashSize = 3
	b.meshcoreChunkDelay = 20 * time.Millisecond

	long := func(c string) string { return strings.Repeat(c, meshcoreMaxChunkBytes*2) }
	var wg sync.WaitGroup
	for _, msg := range []discord.IncomingMessage{
		{ChannelID: "chan-1", AuthorName: "Alice", Content: long("a")},
		{ChannelID: "chan-1", AuthorName: "Bob", Content: long("b")},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.handleDiscordMessage(msg)
		}()
	}
	wg.Wait()

	wantChunks := 2 * len(composeChunks("Alice", long("a"), meshcoreMaxChunkBytes))
	var senders []string
	for i := 0; i < wantChunks; i++ {
		select {
		case raw := <-sent:
			pkt, err := meshcore.ParsePacket(raw)
			if err != nil {
				t.Fatalf("ParsePacket: %v", err)
			}
			dec, ok := meshcore.DecryptGroupText(m.secret, pkt.Payload)
			if !ok {
				t.Fatal("decrypt failed")
			}
			sender, _ := splitSenderText(dec.Text)
			senders = append(senders, strings.Fields(sender)[0])
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out after %d/%d chunks", i, wantChunks)
		}
	}
	switches := 0
	for i := 1; i < len(senders); i++ {
		if senders[i] != senders[i-1] {
			switches++
		}
	}
	if switches != 1 {
		t.Errorf("chunks interleaved between messages: %v", senders)
	}
}

// The Discord handler must hand off and return rather than sit in
// busy_wait / chunk delays on the gateway's goroutine.
func TestBridge_HandleDiscordMessage_DoesNotBlockOnMeshSend(t *testing.T) {
	session, sent := dialTestSession(t)
	m, _ := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.meshcoreChunkDelay = 500 * time.Millisecond

	start := time.Now()
	b.handleDiscordMessage(discord.IncomingMessage{ChannelID: "chan-1", AuthorName: "Alice", Content: strings.Repeat("x", meshcoreMaxChunkBytes*2)})
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("handleDiscordMessage blocked for %s", d)
	}
	<-sent
}

// A retry timer and a fresh send must not both pass listen-before-talk
// and transmit back to back: transmitMeshcore calls are serialised.
func TestBridge_TransmitMeshcore_SerialisesConcurrentCalls(t *testing.T) {
	session, sent := dialTestSession(t)
	m, _ := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.busyWaitEnabled = true
	b.busyMaxDefer = 2 * time.Second
	b.minTxSpacing = 300 * time.Millisecond

	var wg sync.WaitGroup
	for _, text := range []string{"Alice: one", "Bob: two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.transmitMeshcore(m, text, uint32(time.Now().Unix()), 1, 1, 0)
		}()
	}
	var at []time.Time
	for i := 0; i < 2; i++ {
		select {
		case <-sent:
			at = append(at, time.Now())
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for sends")
		}
	}
	wg.Wait()
	if gap := at[1].Sub(at[0]); gap < 250*time.Millisecond {
		t.Errorf("second transmit came %s after the first; want >= min_tx_spacing", gap)
	}
}

// A Discord message dropped because the send queue is full still gets a
// ⚠️ when delivery reactions are on, rather than no feedback at all.
func TestBridge_DroppedDiscordMessageGetsWarningReaction(t *testing.T) {
	m, _ := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)
	fr := &fakeReactor{}
	b.react = fr

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	b.enqueueRelay("general", func() { <-block })
	for i := 0; i < relayQueueCap; i++ {
		b.enqueueRelay("general", func() {})
	}

	b.handleDiscordMessage(discordMsg("m1", "hello"))
	fr.waitFor(t, "m1 +"+reactUnconfirmed)
}
