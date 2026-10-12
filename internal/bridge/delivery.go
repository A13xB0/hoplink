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
// A send counts as delivered once the copies heard satisfy the sending
// mapping's rule (see confirmed): at least meshcore.min_repeaters distinct
// non-ignored repeaters, one of which is on meshcore.success_repeaters if
// that's set. retry_on_no_repeat retransmits while it isn't (up to
// meshcore.max_retries). Suppressing our own echo from being reposted is
// separate — markOwnPacket's job.

// delivery is the tracked state of one outbound chunk (all its attempts).
type delivery struct {
	bridge     string
	chunkIdx   int
	chunkTotal int
	firstSent  time.Time
	attempts   int
	copies     int             // relayed copies heard on the raw log
	repeaters  []string        // distinct hop hashes (hex) seen in any copy's path, first-heard order
	ignore     [][]byte        // the sending mapping's meshcore.ignore_repeat_from
	success    []string        // the sending mapping's meshcore.success_repeaters (lowercase hex)
	minRepeat  int             // the sending mapping's meshcore.min_repeaters
	receipt    *discordReceipt // originating Discord message's delivery reactions; nil if none / once settled
	reported   bool            // summary already logged; later copies only count
}

// trackDelivery records an attempt to send the packet hashed h — creating
// the record on the first attempt.
func (b *Bridge) trackDelivery(h meshcore.PacketHash, m *mapping, chunkIdx, chunkTotal int, rc *discordReceipt) {
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
			success:    m.successRepeaters,
			minRepeat:  m.minRepeaters,
			receipt:    rc,
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

// deliveryConfirmed reports whether h's tracked send currently satisfies its
// delivery rule. An untracked hash (never sent, or already swept) reports
// false.
func (b *Bridge) deliveryConfirmed(h meshcore.PacketHash) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.deliveries[h]
	return ok && d.confirmed()
}

// deliveryRule describes h's delivery rule for logging ("" if untracked).
func (b *Bridge) deliveryRule(h meshcore.PacketHash) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d, ok := b.deliveries[h]; ok {
		return d.rule()
	}
	return ""
}

// proven returns the non-ignored repeaters heard relaying d, in first-heard
// order, and the ignored ones separately. Caller holds b.mu.
func (d *delivery) proven() (proven, ignored []string) {
	for _, r := range d.repeaters {
		if d.isIgnored(r) {
			ignored = append(ignored, r)
		} else {
			proven = append(proven, r)
		}
	}
	return proven, ignored
}

// confirmed applies d's delivery rule. Caller holds b.mu.
func (d *delivery) confirmed() bool {
	proven, _ := d.proven()
	if len(proven) < d.minRepeaters() {
		return false
	}
	if len(d.success) == 0 {
		return true
	}
	for _, r := range proven {
		if d.isSuccessRepeater(r) {
			return true
		}
	}
	return false
}

func (d *delivery) minRepeaters() int {
	if d.minRepeat < 1 {
		return 1
	}
	return d.minRepeat
}

// isSuccessRepeater matches hexHop against success_repeaters by prefix
// either way, so a configured 3-byte hash also matches the same repeater
// heard at a 2-byte path hash width (and vice versa).
func (d *delivery) isSuccessRepeater(hexHop string) bool {
	for _, s := range d.success {
		if strings.HasPrefix(hexHop, s) || strings.HasPrefix(s, hexHop) {
			return true
		}
	}
	return false
}

// rule describes d's delivery rule, e.g. "2 repeaters including one of
// [a1b2c3]".
func (d *delivery) rule() string {
	n := d.minRepeaters()
	need := fmt.Sprintf("%d %s", n, plural(n, "repeater", "repeaters"))
	if len(d.success) > 0 {
		need += fmt.Sprintf(" including one of [%s]", strings.Join(d.success, ", "))
	}
	return need
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
	rc, confirmed := d.receipt, d.confirmed()
	d.receipt = nil
	b.mu.Unlock()
	logfMeshcore("%s", line)
	rc.settle(confirmed)
}

// summary renders d as a log line. Caller holds b.mu.
func (d *delivery) summary(h meshcore.PacketHash) string {
	head := fmt.Sprintf("TX bridge %q: chunk %d/%d (packet %s)", d.bridge, d.chunkIdx, d.chunkTotal, h)
	proven, ignored := d.proven()
	attempts := fmt.Sprintf("%d %s", d.attempts, plural(d.attempts, "attempt", "attempts"))
	heard := fmt.Sprintf("%d %s heard via %d %s [%s]",
		d.copies, plural(d.copies, "copy", "copies"), len(proven), plural(len(proven), "repeater", "repeaters"), strings.Join(proven, ", "))
	also := ""
	if len(ignored) > 0 {
		also = fmt.Sprintf(" (also via ignored: %s)", strings.Join(ignored, ", "))
	}
	switch {
	case d.confirmed():
		return fmt.Sprintf("%s delivered: %s after %s%s", head, heard, attempts, also)
	case len(proven) > 0:
		return fmt.Sprintf("%s NOT confirmed: %s, but the rule needs %s, after %s%s", head, heard, d.rule(), attempts, also)
	case len(ignored) > 0:
		return fmt.Sprintf("%s NOT confirmed: heard only via ignored repeaters [%s] after %s", head, strings.Join(ignored, ", "), attempts)
	case d.copies > 0:
		return fmt.Sprintf("%s NOT confirmed: %d %s heard with no path info after %s", head, d.copies, plural(d.copies, "copy", "copies"), attempts)
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
