package bridge

import (
	"testing"
	"time"

	"github.com/hectospark/hoplink/internal/meshcore"
)

// buildScopedLogRxData mirrors buildLogRxData but produces a
// ROUTE_TYPE_TRANSPORT_FLOOD packet scoped to scopeName, the same way real
// scoped sends are constructed (see meshcore.Session.SendChannelMessage).
func buildScopedLogRxData(t *testing.T, secret []byte, timestamp uint32, text, scopeName string) meshcore.LogRxData {
	t.Helper()
	payload, err := meshcore.BuildGroupTextPayload(secret, timestamp, 0, text)
	if err != nil {
		t.Fatalf("BuildGroupTextPayload: %v", err)
	}
	code0, err := meshcore.CalcTransportCode(meshcore.FloodScopeKey(scopeName), meshcore.PayloadTypeGrpTxt, payload)
	if err != nil {
		t.Fatalf("CalcTransportCode: %v", err)
	}
	pkt, err := meshcore.BuildPacket(meshcore.Packet{
		Route:          meshcore.RouteTransportFlood,
		PayloadType:    meshcore.PayloadTypeGrpTxt,
		Payload:        payload,
		TransportCodes: meshcore.EncodeTransportCodes(code0, 0),
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

func TestBridge_HandleMeshcorePacket_RxScopes_AcceptsMatchingScope(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	m.rxScopes = []string{"myregion"}
	b := newTestBridge(m)

	lrx := buildScopedLogRxData(t, m.secret, 1000, "Alice: hi", "myregion")
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-posts:
		if p.content != "hi" {
			t.Errorf("got post %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the in-scope packet to be delivered")
	}
}

func TestBridge_HandleMeshcorePacket_RxScopes_RejectsWrongScope(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	m.rxScopes = []string{"myregion"}
	b := newTestBridge(m)

	lrx := buildScopedLogRxData(t, m.secret, 1000, "Alice: hi", "otherregion")
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-posts:
		t.Fatalf("expected an out-of-scope packet to be rejected, got %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestBridge_HandleMeshcorePacket_RxScopes_RejectsUnscopedWhenConfigured(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general")
	m.rxScopes = []string{"myregion"}
	b := newTestBridge(m)

	lrx := buildLogRxData(t, m.secret, 1000, "Alice: hi") // unscoped, plain ROUTE_TYPE_FLOOD
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-posts:
		t.Fatalf("expected an unscoped packet to be rejected once rx_scopes is configured, got %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestBridge_HandleMeshcorePacket_RxScopes_SiblingsFilteredIndependently
// guards against a bug where rx_scopes was resolved once per packet (from
// whichever sibling mapping's secret happened to decrypt first) rather than
// per mapping: two bridges sharing the same MeshCore channel but configuring
// different rx_scopes overrides must each be filtered against their own
// setting, not whichever sibling's setting was checked first.
func TestBridge_HandleMeshcorePacket_RxScopes_SiblingsFilteredIndependently(t *testing.T) {
	permissive, permissivePosts := newTestMapping(t, "guild-a", "#shared") // rxScopes left empty: accepts any scope
	restrictive, restrictivePosts := newTestMapping(t, "guild-b", "#shared")
	restrictive.rxScopes = []string{"myregion"}
	b := newTestBridge(permissive, restrictive)

	lrx := buildScopedLogRxData(t, permissive.secret, 1000, "Alice: hi", "otherregion")
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-permissivePosts:
		if p.content != "hi" {
			t.Errorf("got post %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected the permissive (no rx_scopes) sibling to still receive an out-of-scope message")
	}
	select {
	case p := <-restrictivePosts:
		t.Fatalf("expected the restrictive sibling's own rx_scopes to reject this message, got %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestBridge_HandleMeshcorePacket_RxScopes_UnsetAcceptsAnyScope(t *testing.T) {
	m, posts := newTestMapping(t, "general", "#general") // rxScopes left empty (default)
	b := newTestBridge(m)

	lrx := buildScopedLogRxData(t, m.secret, 1000, "Alice: hi", "anyregion")
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-posts:
		if p.content != "hi" {
			t.Errorf("got post %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery — rx_scopes unset should accept every scope")
	}
}

// shortScopeHold shortens scopeHoldWait for one test.
func shortScopeHold(t *testing.T, d time.Duration) {
	t.Helper()
	orig := scopeHoldWait
	scopeHoldWait = d
	t.Cleanup(func() { scopeHoldWait = orig })
}

func syncCopy(b *Bridge, m *mapping, ts uint32, text string) {
	b.hashBySlot = map[byte]byte{5: m.channelHash}
	b.handleMeshcoreChannelMessage(meshcore.ChannelMessage{SlotIndex: 5, TimestampUnix: ts, Text: text})
}

func expectNoPost(t *testing.T, posts chan webhookPost, within time.Duration, why string) {
	t.Helper()
	select {
	case p := <-posts:
		t.Fatalf("%s, got %+v", why, p)
	case <-time.After(within):
	}
}

// The bypass this guards: a message in a scope rx_scopes rejects reached
// the sync path first, was relayed (sync carries no scope to check), and
// its raw-log copy was then just a duplicate that never got scope-checked.
func TestBridge_RxScopes_SyncFirstWrongScopeIsNotRelayed(t *testing.T) {
	shortScopeHold(t, 500*time.Millisecond)
	m, posts := newTestMapping(t, "general", "#general")
	m.rxScopes = []string{"sco-admin"}
	b := newTestBridge(m)
	t.Cleanup(b.Close)

	syncCopy(b, m, 1000, "Alice: hi")
	b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, 1000, "Alice: hi", "sco"))

	expectNoPost(t, posts, 800*time.Millisecond, "a sync-first message in a rejected scope must not be relayed")
}

func TestBridge_RxScopes_SyncFirstAllowedScopeIsRelayedOnce(t *testing.T) {
	shortScopeHold(t, 2*time.Second)
	m, posts := newTestMapping(t, "general", "#general")
	m.rxScopes = []string{"fif", "sco-admin"}
	b := newTestBridge(m)
	t.Cleanup(b.Close)

	syncCopy(b, m, 1000, "Alice: hi")
	expectNoPost(t, posts, 150*time.Millisecond, "a sync copy must wait for its scope")
	b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, 1000, "Alice: hi", "fif"))

	select {
	case p := <-posts:
		if p.content != "hi" {
			t.Errorf("got post %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected the held message to be relayed once its raw-log copy proved an allowed scope")
	}
	b.handleMeshcorePacket(buildScopedLogRxData(t, m.secret, 1000, "Alice: hi", "fif")) // another relayed copy
	expectNoPost(t, posts, 2500*time.Millisecond, "the message must be relayed exactly once, including after the hold expires")
}

func TestBridge_RxScopes_SyncOnlyIsDroppedAndLogged(t *testing.T) {
	shortScopeHold(t, 200*time.Millisecond)
	buf := captureLog(t)
	m, posts := newTestMapping(t, "general", "#general")
	m.rxScopes = []string{"sco-admin"}
	b := newTestBridge(m)
	t.Cleanup(b.Close)

	syncCopy(b, m, 1000, "Alice: hi")

	expectNoPost(t, posts, 500*time.Millisecond, "a message whose scope was never proven must not be relayed")
	waitForLog(t, buf, `arrived only via device sync`)
}

func TestBridge_RxScopes_SyncWithoutRxScopesIsNotHeld(t *testing.T) {
	shortScopeHold(t, 5*time.Second)
	m, posts := newTestMapping(t, "general", "#general") // no rx_scopes: accept anything, immediately
	b := newTestBridge(m)
	t.Cleanup(b.Close)

	syncCopy(b, m, 1000, "Alice: hi")

	select {
	case p := <-posts:
		if p.content != "hi" {
			t.Errorf("got post %+v", p)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("a bridge without rx_scopes must relay a sync copy straight away")
	}
}

// Only the sibling that sets rx_scopes waits; the permissive one relays the
// sync copy at once, and the raw-log copy then settles the held one.
func TestBridge_RxScopes_SyncHoldIsPerSibling(t *testing.T) {
	shortScopeHold(t, 2*time.Second)
	permissive, permissivePosts := newTestMapping(t, "guild-a", "#shared")
	restrictive, restrictivePosts := newTestMapping(t, "guild-b", "#shared")
	restrictive.rxScopes = []string{"sco-admin"}
	b := newTestBridge(permissive, restrictive)
	t.Cleanup(b.Close)

	syncCopy(b, permissive, 1000, "Alice: hi")
	select {
	case <-permissivePosts:
	case <-time.After(1 * time.Second):
		t.Fatal("the permissive sibling should relay the sync copy immediately")
	}
	b.handleMeshcorePacket(buildScopedLogRxData(t, permissive.secret, 1000, "Alice: hi", "sco-admin"))
	select {
	case <-restrictivePosts:
	case <-time.After(2 * time.Second):
		t.Fatal("the restrictive sibling should relay once the raw-log copy proves sco-admin")
	}
	expectNoPost(t, permissivePosts, 300*time.Millisecond, "the permissive sibling must not get the message twice")
}
