package bridge

import (
	"time"

	"github.com/hectospark/hoplink/internal/meshcore"
)

// scopeNoticeBridgeGap is the minimum time between any two scope notices
// from one bridge, on top of each sender's own cooldown — so a busy channel
// in a rejected scope can't turn hoplink into a stream of replies.
const scopeNoticeBridgeGap = time.Minute

// maxNoticeCooldown bounds how long noticeSent entries are kept (the
// largest cooldown_s config allows).
const maxNoticeCooldown = 24 * time.Hour

// maybeScopeNotice replies on m's channel when pkt, a message from sender
// that m's rx_scopes rejected, was flooded in one of m's scope_notice
// scopes. The reply is flooded in the notice's reply scope, at most once
// per sender per cooldown and once per scopeNoticeBridgeGap per bridge.
func (b *Bridge) maybeScopeNotice(m *mapping, pkt *meshcore.Packet, sender string) {
	n := m.scopeNotice
	if n == nil || pkt == nil {
		return
	}
	scope, ok, err := pkt.MatchScope(n.Scopes)
	if err != nil || !ok {
		return // unscoped, or a rejected scope with no notice: drop silently
	}

	now := time.Now()
	senderKey := m.cfg.Name + "\x00" + sender
	b.mu.Lock()
	if b.noticeSent == nil {
		b.noticeSent = make(map[string]time.Time)
		b.noticeLast = make(map[string]time.Time)
	}
	last, sentBefore := b.noticeSent[senderKey]
	limited := sentBefore && now.Sub(last) < n.Cooldown()
	busy := now.Sub(b.noticeLast[m.cfg.Name]) < scopeNoticeBridgeGap
	if !limited && !busy {
		b.noticeSent[senderKey] = now
		b.noticeLast[m.cfg.Name] = now
	}
	b.mu.Unlock()

	if limited || busy {
		logfMeshcore("RX bridge %q: not relaying a message from %q sent in scope %q (rx_scopes %v); notice skipped by rate limit", m.cfg.Name, sender, scope, m.rxScopes)
		return
	}
	logfMeshcore("RX bridge %q: not relaying a message from %q sent in scope %q (rx_scopes %v); sending scope notice", m.cfg.Name, sender, scope, m.rxScopes)
	text := scopeNoticeText(n.ResolvedName(), sender, n.Message)
	b.enqueueRelay(m.cfg.Name, func() {
		b.transmitMeshcoreScoped(m, m.noticeScopeKey, text, uint32(time.Now().Unix()), 1, 1, 0, nil)
	})
}

// scopeNoticeText composes the notice as one MeshCore message, mentioning
// sender when that still fits.
func scopeNoticeText(name, sender, message string) string {
	if sender != "" {
		if t := name + ": @[" + sender + "] " + message; len(t) <= meshcoreMaxChunkBytes {
			return t
		}
	}
	return name + ": " + message
}
