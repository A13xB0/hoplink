package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/hectospark/hoplink/internal/config"
	"github.com/hectospark/hoplink/internal/meshcore"
)

const testNoticeMsg = "Not relayed to Meshtastic: switch your region to sco-admin to reach LongFast."

// newScopeNoticeBridge is the longfast setup: rx_scopes [fif, sco-admin],
// a notice for sco, replies in sco-admin.
func newScopeNoticeBridge(t *testing.T) (*Bridge, *mapping, chan []byte, chan webhookPost) {
	t.Helper()
	b, m, sent, posts := newDeliveryTestBridge(t)
	m.rxScopes = []string{"fif", "sco-admin"}
	m.scopeNotice = &config.ScopeNotice{Scopes: []string{"sco"}, Message: testNoticeMsg}
	m.noticeScopeKey = meshcore.FloodScopeKey("sco-admin")
	return b, m, sent, posts
}

// expectNotice waits for one transmitted packet and checks it is the notice
// for sender, flooded in sco-admin.
func expectNotice(t *testing.T, m *mapping, sent chan []byte, sender string) []byte {
	t.Helper()
	select {
	case raw := <-sent:
		pkt, err := meshcore.ParsePacket(raw)
		if err != nil {
			t.Fatalf("ParsePacket: %v", err)
		}
		if scope, ok, _ := pkt.MatchScope([]string{"sco", "fif", "sco-admin"}); !ok || scope != "sco-admin" {
			t.Errorf("notice flooded in scope %q (matched %v), want sco-admin", scope, ok)
		}
		dec, ok := meshcore.DecryptGroupText(m.secret, pkt.Payload)
		if !ok {
			t.Fatal("notice didn't decrypt on the bridge's channel")
		}
		if want := "hoplink: @[" + sender + "] " + testNoticeMsg; dec.Text != want {
			t.Errorf("notice text = %q, want %q", dec.Text, want)
		}
		return raw
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the scope notice")
	}
	return nil
}

func expectNoSend(t *testing.T, sent chan []byte, within time.Duration, why string) {
	t.Helper()
	select {
	case raw := <-sent:
		t.Fatalf("%s, but a packet was sent (%d bytes)", why, len(raw))
	case <-time.After(within):
	}
}

func TestScopeNotice_RejectedScopeGetsOneNoticeAndIsNotRelayed(t *testing.T) {
	b, m, sent, posts := newScopeNoticeBridge(t)

	b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, nextTS(), "Bob: anyone on longfast?", "sco"))

	expectNotice(t, m, sent, "Bob")
	expectNoPost(t, posts, 200*time.Millisecond, "a sco message must not be relayed")
}

func TestScopeNotice_SameSenderWithinCooldownGetsNoSecondNotice(t *testing.T) {
	b, m, sent, _ := newScopeNoticeBridge(t)

	b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, nextTS(), "Bob: one", "sco"))
	expectNotice(t, m, sent, "Bob")
	b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, nextTS(), "Bob: two", "sco"))
	expectNoSend(t, sent, 300*time.Millisecond, "the cooldown should hold back a second notice to Bob")
}

func TestScopeNotice_BridgeWideGapLimitsNoticesToDifferentSenders(t *testing.T) {
	b, m, sent, _ := newScopeNoticeBridge(t)

	b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, nextTS(), "Bob: hi", "sco"))
	expectNotice(t, m, sent, "Bob")
	b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, nextTS(), "Carol: hi", "sco"))
	expectNoSend(t, sent, 300*time.Millisecond, "a second notice within a minute of the first should be held back")
}

func TestScopeNotice_AllowedScopesAreRelayedWithoutNotice(t *testing.T) {
	for _, scope := range []string{"fif", "sco-admin"} {
		t.Run(scope, func(t *testing.T) {
			b, m, sent, posts := newScopeNoticeBridge(t)
			b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, nextTS(), "Bob: hi", scope))
			select {
			case p := <-posts:
				if p.content != "hi" {
					t.Errorf("got post %+v", p)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("a %s message should be relayed", scope)
			}
			expectNoSend(t, sent, 200*time.Millisecond, "an allowed message needs no notice")
		})
	}
}

func TestScopeNotice_OtherRejectedMessagesAreDroppedSilently(t *testing.T) {
	b, m, sent, posts := newScopeNoticeBridge(t)

	b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, nextTS(), "Bob: hi", "eng")) // rejected, not a notice scope
	b.handleMeshcorePacket(buildLogRxData(t, m.secret, nextTS(), "Carol: hi"))            // unscoped

	expectNoSend(t, sent, 300*time.Millisecond, "only scope_notice scopes get a notice")
	expectNoPost(t, posts, 10*time.Millisecond, "rejected messages must not be relayed")
}

func TestScopeNotice_OffByDefault(t *testing.T) {
	b, m, sent, _ := newDeliveryTestBridge(t)
	m.rxScopes = []string{"sco-admin"} // filter on, no scope_notice

	b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, nextTS(), "Bob: hi", "sco"))

	expectNoSend(t, sent, 300*time.Millisecond, "without scope_notice nothing is sent")
}

// A sco message whose sync copy arrives first is held, then rejected (and
// noticed) when its raw-log copy shows the scope.
func TestScopeNotice_SyncFirstStillGetsNotice(t *testing.T) {
	shortScopeHold(t, 2*time.Second)
	b, m, sent, posts := newScopeNoticeBridge(t)
	ts := nextTS()

	syncCopy(b, m, ts, "Bob: hi")
	b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, ts, "Bob: hi", "sco"))

	expectNotice(t, m, sent, "Bob")
	expectNoPost(t, posts, 200*time.Millisecond, "the held sco message must not be relayed")
}

// The notice's own echo, relayed back by a repeater, is ours: never relayed
// and never noticed again.
func TestScopeNotice_EchoOfNoticeIsNotRelayed(t *testing.T) {
	b, m, sent, posts := newScopeNoticeBridge(t)

	b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, nextTS(), "Bob: hi", "sco"))
	raw := expectNotice(t, m, sent, "Bob")
	b.handleMeshcorePacket(relayedCopy(t, raw, "a1b2c3"))

	expectNoPost(t, posts, 300*time.Millisecond, "the notice's echo must not be relayed")
}

func TestScopeNoticeText_DropsMentionWhenTooLong(t *testing.T) {
	long := strings.Repeat("x", 60)
	if got := scopeNoticeText("hoplink", "Bob", "short"); got != "hoplink: @[Bob] short" {
		t.Errorf("got %q", got)
	}
	if got := scopeNoticeText("hoplink", strings.Repeat("B", 40), long+long); got != "hoplink: "+long+long {
		t.Errorf("an over-long notice should drop the mention, got %q", got)
	}
}
