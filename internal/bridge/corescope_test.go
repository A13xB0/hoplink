package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/hectospark/hoplink/internal/corescope"
)

func obsFor(t *testing.T, raw []byte, observerName string, hops ...string) corescope.Observation {
	t.Helper()
	return corescope.Observation{Hash: mustPacketHash(t, raw), ObserverID: "id-" + observerName, ObserverName: observerName, Hops: hops}
}

// The case local echoes can't cover: a repeater relayed our packet but we
// never heard it (its rebroadcast collided at our antenna, or it's out of
// our range). A CoreScope observer that did hear it proves the relay, so no
// pointless retry fires.
func TestCoreScope_RemoteObservationConfirmsAndPreventsRetry(t *testing.T) {
	buf := captureLog(t)
	b, m, sent, _ := newDeliveryTestBridge(t)
	b.meshcoreRetryOnNoRepeat = true

	b.transmitMeshcore(m, "Alice: hello", nextTS(), 1, 1, 0)
	raw := <-sent
	b.HandleCoreScopeObservation(obsFor(t, raw, "Townhall", "a1a1a1", "b2b2b2"))

	expectSends(t, sent, 0, time.Second)
	line := summaryLine(t, waitForLog(t, buf, "(packet "+mustPacketHash(t, raw)+") delivered"), raw)
	for _, want := range []string{"via 2 repeaters [a1a1a1, b2b2b2]", "seen by 1 CoreScope observer [Townhall]"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary %q missing %q", line, want)
		}
	}
}

func TestCoreScope_SuccessObserverConfirmsWithoutAnyRepeater(t *testing.T) {
	buf := captureLog(t)
	b, m, sent, _ := newDeliveryTestBridge(t)
	b.meshcoreRetryOnNoRepeat = true
	m.successObservers = []string{"far-side"}
	m.successRepeaters = []string{"c3c3c3"}

	b.transmitMeshcore(m, "Alice: hello", nextTS(), 1, 1, 0)
	raw := <-sent
	b.HandleCoreScopeObservation(obsFor(t, raw, "Far-Side")) // heard us directly: no hops

	expectSends(t, sent, 0, time.Second)
	line := summaryLine(t, waitForLog(t, buf, "(packet "+mustPacketHash(t, raw)+") delivered"), raw)
	if !strings.Contains(line, "no relayed copy heard locally") {
		t.Errorf("summary = %q", line)
	}
}

func TestCoreScope_OtherObserversDontSatisfySuccessObservers(t *testing.T) {
	buf := captureLog(t)
	b, m, sent, _ := newDeliveryTestBridge(t)
	m.successObservers = []string{"far-side"}
	m.successRepeaters = []string{"c3c3c3"}

	b.transmitMeshcore(m, "Alice: hello", nextTS(), 1, 1, 0)
	raw := <-sent
	b.HandleCoreScopeObservation(obsFor(t, raw, "Nearby", "a1a1a1"))

	line := summaryLine(t, waitForLog(t, buf, "(packet "+mustPacketHash(t, raw)+") NOT confirmed"), raw)
	if !strings.Contains(line, "or a report from CoreScope observer [far-side]") {
		t.Errorf("summary = %q", line)
	}
}

func TestCoreScope_LateObservationLogsLateConfirmation(t *testing.T) {
	buf := captureLog(t)
	b, m, sent, _ := newDeliveryTestBridge(t)

	b.transmitMeshcore(m, "Alice: hello", nextTS(), 1, 1, 0)
	raw := <-sent
	waitForLog(t, buf, "(packet "+mustPacketHash(t, raw)+") NOT confirmed")
	b.HandleCoreScopeObservation(obsFor(t, raw, "Townhall", "a1a1a1"))
	waitForLog(t, buf, `delivered after all: confirmed late by CoreScope observer "Townhall"`)

	// A second late observation must not log it again.
	b.HandleCoreScopeObservation(obsFor(t, raw, "Hilltop", "b2b2b2"))
	if n := strings.Count(buf.String(), "delivered after all"); n != 1 {
		t.Errorf("late confirmation logged %d times, want 1", n)
	}
}

func TestCoreScope_UnrelatedAndMalformedHashesIgnored(t *testing.T) {
	b, m, sent, _ := newDeliveryTestBridge(t)
	b.transmitMeshcore(m, "Alice: hello", nextTS(), 1, 1, 0)
	raw := <-sent
	h := mustPacketHash(t, raw)

	b.HandleCoreScopeObservation(corescope.Observation{Hash: "0000000000000000", Hops: []string{"a1a1a1"}})
	b.HandleCoreScopeObservation(corescope.Observation{Hash: "zz", Hops: []string{"a1a1a1"}})
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, d := range b.deliveries {
		if k.String() == h && (len(d.repeaters) != 0 || len(d.observers) != 0) {
			t.Errorf("our record picked up evidence from someone else's packet: %+v", d)
		}
	}
}
