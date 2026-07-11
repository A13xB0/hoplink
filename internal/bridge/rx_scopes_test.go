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
