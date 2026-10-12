package bridge

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/hectospark/hoplink/internal/meshcore"
)

// relayedCopy returns raw as heard back after relaying through hops (hex hop
// hashes, all the same width). The path width doesn't affect the packet hash.
func relayedCopy(t *testing.T, raw []byte, hops ...string) meshcore.LogRxData {
	t.Helper()
	pkt, err := meshcore.ParsePacket(raw)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	pkt.Path = nil
	for i, h := range hops {
		b, err := hex.DecodeString(h)
		if err != nil || (i > 0 && len(b) != pkt.HashSize) {
			t.Fatalf("bad test hop %q for hash size %d", h, pkt.HashSize)
		}
		pkt.HashSize = len(b) // the hop width the test's repeaters used
		pkt.Path = append(pkt.Path, b...)
	}
	return meshcore.LogRxData{Packet: pkt, RawLen: len(raw) + len(pkt.Path)}
}

func waitForLog(t *testing.T, buf *syncBuffer, substr string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := buf.String(); strings.Contains(s, substr) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for log containing %q; logs:\n%s", substr, buf.String())
	return ""
}

func summaryLine(t *testing.T, logs string) string {
	t.Helper()
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "(packet ") && (strings.Contains(l, " delivered:") || strings.Contains(l, "NOT confirmed")) {
			return l
		}
	}
	t.Fatalf("no delivery summary in logs:\n%s", logs)
	return ""
}

func newDeliveryTestBridge(t *testing.T) (*Bridge, *mapping, chan []byte, chan webhookPost) {
	t.Helper()
	session, sent := dialTestSession(t)
	m, posts := newDualBackendMapping(t, "")
	m.meshtasticEnabled = false
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.hashSize = 3
	b.meshcoreRetryWait = 100 * time.Millisecond
	return b, m, sent, posts
}

func TestDelivery_CountsEveryCopyAndEveryRepeaterInItsPath(t *testing.T) {
	buf := captureLog(t)
	b, m, sent, posts := newDeliveryTestBridge(t)

	b.transmitMeshcore(m, "Alice: hello", 1_700_000_000, 1, 1, 0)
	raw := <-sent
	b.handleMeshcorePacket(relayedCopy(t, raw, "a1a1a1"))
	b.handleMeshcorePacket(relayedCopy(t, raw, "a1a1a1", "b2b2b2", "c3c3c3"))
	b.handleMeshcorePacket(relayedCopy(t, raw, "d4d4d4"))

	line := summaryLine(t, waitForLog(t, buf, "(packet "+mustPacketHash(t, raw)+") delivered"))
	for _, want := range []string{"3 copies", "4 repeaters", "a1a1a1, b2b2b2, c3c3c3, d4d4d4", "after 1 attempt"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary %q missing %q", line, want)
		}
	}
	assertNoPost(t, posts) // every copy is still our own echo
}

func TestDelivery_ReportsNotConfirmedWhenNothingHeard(t *testing.T) {
	buf := captureLog(t)
	b, m, sent, _ := newDeliveryTestBridge(t)

	b.transmitMeshcore(m, "Alice: hello", 1_700_000_000, 1, 1, 0)
	<-sent
	line := summaryLine(t, waitForLog(t, buf, "NOT confirmed"))
	if !strings.Contains(line, "no relayed copy heard after 1 attempt") {
		t.Errorf("summary = %q", line)
	}
}

func TestDelivery_IgnoredRepeatersDontConfirm(t *testing.T) {
	buf := captureLog(t)
	b, m, sent, _ := newDeliveryTestBridge(t)
	m.ignoreRepeatFrom = [][]byte{{0xa1, 0xa1, 0xa1}}

	b.transmitMeshcore(m, "Alice: hello", 1_700_000_000, 1, 1, 0)
	raw := <-sent
	b.handleMeshcorePacket(relayedCopy(t, raw, "a1a1a1"))

	line := summaryLine(t, waitForLog(t, buf, "NOT confirmed"))
	if !strings.Contains(line, "heard only via ignored repeaters [a1a1a1]") {
		t.Errorf("summary = %q", line)
	}
}

func TestDelivery_RetryIsOneRecordWithTwoAttempts(t *testing.T) {
	buf := captureLog(t)
	b, m, sent, _ := newDeliveryTestBridge(t)
	b.meshcoreRetryOnNoRepeat = true

	b.transmitMeshcore(m, "Alice: hello", 1_700_000_000, 1, 1, 0)
	raw := <-sent
	retry := <-sent // nothing heard: retransmitted, byte-identical
	if string(raw) != string(retry) {
		t.Fatal("retransmit should be byte-identical")
	}
	b.handleMeshcorePacket(relayedCopy(t, retry, "b2b2b2"))

	logs := waitForLog(t, buf, "after 2 attempts")
	if n := strings.Count(logs, "(packet "+mustPacketHash(t, raw)+") delivered"); n != 1 {
		t.Errorf("want exactly one summary for the retried packet, got %d:\n%s", n, logs)
	}
	if strings.Contains(logs, "NOT confirmed") {
		t.Errorf("the first attempt should not be reported on its own before the retry:\n%s", logs)
	}
}

func TestDelivery_UnrelatedPacketsAreNotTracked(t *testing.T) {
	b, _, _, _ := newDeliveryTestBridge(t)
	other := meshcore.Packet{PayloadType: meshcore.PayloadTypeGrpTxt, HashSize: 3, Payload: []byte{1, 2, 3}}
	if b.observeOwnCopy(other) {
		t.Error("a packet we never sent must not be treated as our own copy")
	}
}

func TestDelivery_SweepForgetsOldRecords(t *testing.T) {
	b, m, _, _ := newDeliveryTestBridge(t)
	var h meshcore.PacketHash
	b.trackDelivery(h, m, 1, 1)
	b.mu.Lock()
	b.deliveries[h].firstSent = time.Now().Add(-2 * inboundDedupTTL)
	b.mu.Unlock()
	b.sweep()
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.deliveries[h]; ok {
		t.Error("sweep should drop delivery records older than inboundDedupTTL")
	}
}

func mustPacketHash(t *testing.T, raw []byte) string {
	t.Helper()
	pkt, err := meshcore.ParsePacket(raw)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	return pkt.Hash().String()
}

func expectSends(t *testing.T, sent chan []byte, n int, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for i := 0; i < n; i++ {
		select {
		case <-sent:
		case <-deadline:
			t.Fatalf("got %d sends, want %d", i, n)
		}
	}
	select {
	case <-sent:
		t.Fatalf("got more than %d sends", n)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestDeliveryRule_MinRepeatersRetriesUntilEnoughHeard(t *testing.T) {
	buf := captureLog(t)
	b, m, sent, _ := newDeliveryTestBridge(t)
	b.meshcoreRetryOnNoRepeat = true
	m.minRepeaters = 2

	b.transmitMeshcore(m, "Alice: hello", 1_700_000_000, 1, 1, 0)
	raw := <-sent
	b.handleMeshcorePacket(relayedCopy(t, raw, "a1a1a1")) // only one repeater: not enough

	expectSends(t, sent, 1, 2*time.Second) // so it retransmits once
	line := summaryLine(t, waitForLog(t, buf, "NOT confirmed"))
	if !strings.Contains(line, "the rule needs 2 repeaters") || !strings.Contains(line, "after 2 attempts") {
		t.Errorf("summary = %q", line)
	}
}

func TestDeliveryRule_MinRepeatersSatisfiedAcrossCopies(t *testing.T) {
	buf := captureLog(t)
	b, m, sent, _ := newDeliveryTestBridge(t)
	b.meshcoreRetryOnNoRepeat = true
	m.minRepeaters = 2

	b.transmitMeshcore(m, "Alice: hello", 1_700_000_000, 1, 1, 0)
	raw := <-sent
	b.handleMeshcorePacket(relayedCopy(t, raw, "a1a1a1"))
	b.handleMeshcorePacket(relayedCopy(t, raw, "b2b2b2"))

	expectSends(t, sent, 0, time.Second)
	waitForLog(t, buf, "delivered: 2 copies heard via 2 repeaters")
}

func TestDeliveryRule_SuccessRepeatersMustBeAmongThoseHeard(t *testing.T) {
	buf := captureLog(t)
	b, m, sent, _ := newDeliveryTestBridge(t)
	b.meshcoreRetryOnNoRepeat = true
	m.successRepeaters = []string{"c3c3c3"}

	b.transmitMeshcore(m, "Alice: hello", 1_700_000_000, 1, 1, 0)
	raw := <-sent
	b.handleMeshcorePacket(relayedCopy(t, raw, "a1a1a1")) // not the one we need
	retry := <-sent
	// The hilltop repeater shows up deeper in a later copy's path, heard at
	// a 2-byte hop width: matches c3c3c3 by prefix.
	b.handleMeshcorePacket(relayedCopy(t, retry, "c3c3", "a1a1"))

	line := summaryLine(t, waitForLog(t, buf, "delivered"))
	if !strings.Contains(line, "c3c3") || !strings.Contains(line, "after 2 attempts") {
		t.Errorf("summary = %q", line)
	}
}

func TestDeliveryRule_MaxRetries(t *testing.T) {
	buf := captureLog(t)
	b, m, sent, _ := newDeliveryTestBridge(t)
	b.meshcoreRetryOnNoRepeat = true
	b.meshcoreMaxRetries = 3

	b.transmitMeshcore(m, "Alice: hello", 1_700_000_000, 1, 1, 0)
	expectSends(t, sent, 4, 3*time.Second) // original + 3 retries, then stop
	line := summaryLine(t, waitForLog(t, buf, "NOT confirmed"))
	if !strings.Contains(line, "after 4 attempts") {
		t.Errorf("summary = %q", line)
	}
	if !strings.Contains(buf.String(), "attempt 4/4") {
		t.Errorf("TX log should count attempts against max_retries+1:\n%s", buf.String())
	}
}
