package meshtastic

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	generated "github.com/meshnet-gophers/meshtastic-go/meshtastic"
	"google.golang.org/protobuf/proto"
)

// captureLog redirects the standard logger to a buffer for the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	orig := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return buf
}

// lockedBuffer is a bytes.Buffer safe to read while readLoop still logs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForLog polls buf until it contains want or the deadline passes.
func waitForLog(t *testing.T, buf *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("log never contained %q; got:\n%s", want, buf.String())
}

// A broadcast sent without want_ack is aired once and forgotten: if no node
// happens to hear it, it is lost. With want_ack the device retransmits until
// it hears a neighbour rebroadcast it (Meshtastic's implicit ack).
func TestSendTextOnChannel_AsksForAck(t *testing.T) {
	sent := make(chan *generated.MeshPacket, 1)
	s, _ := dialAgainstStandardHandshake(t, func(msg *generated.ToRadio) {
		if p, ok := msg.PayloadVariant.(*generated.ToRadio_Packet); ok {
			sent <- p.Packet
		}
	})
	if err := s.SendTextOnChannel(0, "Alice: hello mesh", 7, 0x1234); err != nil {
		t.Fatalf("SendTextOnChannel: %v", err)
	}
	select {
	case pkt := <-sent:
		if !pkt.WantAck {
			t.Error("WantAck = false, want true so the device retries until a node rebroadcasts it")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the packet never reached the radio")
	}
}

// routingResult is the ROUTING_APP packet a device sends its client for a
// want_ack packet: NONE once a node rebroadcast it, an error otherwise.
func routingResult(requestID uint32, reason generated.Routing_Error) *generated.FromRadio {
	payload, _ := proto.Marshal(&generated.Routing{Variant: &generated.Routing_ErrorReason{ErrorReason: reason}})
	return &generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From: 42,
		To:   42,
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum:   generated.PortNum_ROUTING_APP,
			RequestId: requestID,
			Payload:   payload,
		}},
	}}}
}

func TestSession_LogsWhetherTheMeshRebroadcastOurPacket(t *testing.T) {
	buf := captureLog(t)
	s, radioCh := dialAgainstStandardHandshake(t, nil)
	radio := <-radioCh

	if err := s.SendTextOnChannel(0, "Alice: one", 7, 0xA1); err != nil {
		t.Fatalf("SendTextOnChannel: %v", err)
	}
	if err := s.SendTextOnChannel(0, "Alice: two", 7, 0xA2); err != nil {
		t.Fatalf("SendTextOnChannel: %v", err)
	}
	radio.push(routingResult(0xA1, generated.Routing_NONE))
	waitForLog(t, buf, "TX packet id 0x000000a1 delivered: a node rebroadcast it")

	radio.push(routingResult(0xA2, generated.Routing_MAX_RETRANSMIT))
	waitForLog(t, buf, "TX packet id 0x000000a2 NOT delivered: no node rebroadcast it after the device's retries")
}

// Results for packets this session didn't send (the device's own nodeinfo
// or telemetry, or another client's packets) are not ours to report.
func TestSession_IgnoresRoutingResultsForOtherPackets(t *testing.T) {
	buf := captureLog(t)
	s, radioCh := dialAgainstStandardHandshake(t, nil)
	radio := <-radioCh
	if err := s.SendTextOnChannel(0, "Alice: ours", 7, 0xB1); err != nil {
		t.Fatalf("SendTextOnChannel: %v", err)
	}
	radio.push(routingResult(0xEE, generated.Routing_MAX_RETRANSMIT))
	radio.push(routingResult(0xB1, generated.Routing_NONE))
	waitForLog(t, buf, "TX packet id 0x000000b1 delivered")
	if strings.Contains(buf.String(), "0x000000ee") {
		t.Errorf("logged a routing result for a packet this session did not send:\n%s", buf.String())
	}
}
