package bridge

import (
	"fmt"
	"strings"
	"time"

	"github.com/hectospark/hoplink/internal/meshcore"
)

// Delivery tracking for our own MeshCore transmissions. GRP_TXT floods carry
// no ack, so the evidence that a message actually made it onto the mesh is
// hearing relayed copies of it come back. Each send is keyed by its
// meshcore.PacketHash — identical across every relayed copy and across our
// byte-identical retransmits, and the same identity repeaters and CoreScope
// use — and every copy heard on the raw RF log is recorded, along with every
// repeater in its path (a copy that travelled a1 -> b2 -> c3 proves all three
// relayed it, not just the last hop). One summary line per chunk is logged
// once the send has settled (see reportDelivery).
//
// This is observation only: suppressing our own echo from being reposted is
// markOwnPacket's job, and retry_on_no_repeat's decision still runs off the
// pending-echo accounting in recentOutbound.

// delivery is the tracked state of one outbound chunk (all its attempts).
type delivery struct {
	bridge     string
	chunkIdx   int
	chunkTotal int
	firstSent  time.Time
	attempts   int
	copies     int      // relayed copies heard on the raw log
	repeaters  []string // distinct hop hashes (hex) seen in any copy's path, first-heard order
	ignore     [][]byte // the sending mapping's meshcore.ignore_repeat_from
	reported   bool     // summary already logged; later copies only count
}

// trackDelivery records an attempt to send the packet hashed h — creating
// the record on the first attempt.
func (b *Bridge) trackDelivery(h meshcore.PacketHash, m *mapping, chunkIdx, chunkTotal int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deliveries == nil {
		b.deliveries = make(map[meshcore.PacketHash]*delivery)
	}
	d, ok := b.deliveries[h]
	if !ok {
		d = &delivery{
			bridge:     m.cfg.Name,
			chunkIdx:   chunkIdx,
			chunkTotal: chunkTotal,
			firstSent:  time.Now(),
			ignore:     m.ignoreRepeatFrom,
		}
		b.deliveries[h] = d
	}
	d.attempts++
	d.reported = false
}

// forgetDelivery drops h's record — used when its very first send failed,
// so there's nothing to report.
func (b *Bridge) forgetDelivery(h meshcore.PacketHash) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.deliveries, h)
}

// observeOwnCopy records pkt if it's a relayed copy of one of our tracked
// sends, reporting whether it was. Called for every raw-log GRP_TXT frame
// before (and independently of) decryption and dedup.
func (b *Bridge) observeOwnCopy(pkt meshcore.Packet) bool {
	h := pkt.Hash()
	hops := pkt.Hops()
	b.mu.Lock()
	d, ok := b.deliveries[h]
	if !ok {
		b.mu.Unlock()
		return false
	}
	d.copies++
	for _, hop := range hops {
		hx := fmt.Sprintf("%x", hop)
		if !containsString(d.repeaters, hx) {
			d.repeaters = append(d.repeaters, hx)
		}
	}
	name, copies, late := d.bridge, d.copies, d.reported
	b.mu.Unlock()

	when := ""
	if late {
		when = " (after the delivery summary)"
	}
	b.debugMeshcore("RX copy %d of our packet %s for bridge %q%s, relayed via %s", copies, h, name, when, formatRepeaterPath(pkt.Path, pkt.HashSize))
	return true
}

// reportDelivery logs h's one-line delivery summary, once per settled send
// (a retransmit re-arms it). The record is kept until sweep so stragglers
// are still recognised as ours.
func (b *Bridge) reportDelivery(h meshcore.PacketHash) {
	b.mu.Lock()
	d, ok := b.deliveries[h]
	if !ok || d.reported {
		b.mu.Unlock()
		return
	}
	d.reported = true
	line := d.summary(h)
	b.mu.Unlock()
	logfMeshcore("%s", line)
}

// summary renders d as a log line. Caller holds b.mu.
func (d *delivery) summary(h meshcore.PacketHash) string {
	head := fmt.Sprintf("TX bridge %q: chunk %d/%d (packet %s)", d.bridge, d.chunkIdx, d.chunkTotal, h)
	var proven, ignored []string
	for _, r := range d.repeaters {
		if d.isIgnored(r) {
			ignored = append(ignored, r)
		} else {
			proven = append(proven, r)
		}
	}
	attempts := fmt.Sprintf("%d attempt", d.attempts)
	if d.attempts != 1 {
		attempts += "s"
	}
	switch {
	case len(proven) > 0:
		s := fmt.Sprintf("%s repeated: %d %s heard via %d %s [%s] after %s",
			head, d.copies, plural(d.copies, "copy", "copies"), len(proven), plural(len(proven), "repeater", "repeaters"), strings.Join(proven, ", "), attempts)
		if len(ignored) > 0 {
			s += fmt.Sprintf(" (also via ignored: %s)", strings.Join(ignored, ", "))
		}
		return s
	case len(ignored) > 0:
		return fmt.Sprintf("%s NOT confirmed: heard only via ignored repeaters [%s] after %s", head, strings.Join(ignored, ", "), attempts)
	case d.copies > 0:
		return fmt.Sprintf("%s repeated: %d %s heard with no path info after %s", head, d.copies, plural(d.copies, "copy", "copies"), attempts)
	default:
		return fmt.Sprintf("%s NOT confirmed: no relayed copy heard after %s", head, attempts)
	}
}

func (d *delivery) isIgnored(hexHop string) bool {
	for _, ig := range d.ignore {
		if fmt.Sprintf("%x", ig) == hexHop {
			return true
		}
	}
	return false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
