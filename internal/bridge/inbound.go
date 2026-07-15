package bridge

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/hectospark/hoplink/internal/meshcore"
	"github.com/hectospark/hoplink/internal/meshtastic"
)

// handleMeshcorePacket is called for every RF packet the MeshCore radio
// hears (the raw-log inbound path — see meshcore.Session.LogRxFrames). It
// tries to decrypt GRP_TXT payloads against each meshcore-enabled bridge's
// secret; a successful decrypt identifies the channel itself (channelHash),
// not a single "owning" bridge, since multiple bridges may share the same
// MeshCore channel (e.g. relaying one mesh channel into two different
// Discord guilds) — rx_scopes filtering is therefore deferred to
// deliverMeshcoreChannelText, which evaluates it per sibling mapping rather
// than once here (siblings sharing this channel may configure different
// rx_scopes overrides).
func (b *Bridge) handleMeshcorePacket(lrx meshcore.LogRxData) {
	if lrx.Packet.PayloadType != meshcore.PayloadTypeGrpTxt {
		return
	}

	var dec meshcore.GroupTextDecrypt
	var channelHash byte
	found := false
	for _, m := range b.byName {
		if !m.meshcoreEnabled {
			continue
		}
		d, ok := meshcore.DecryptGroupText(m.secret, lrx.Packet.Payload)
		if !ok {
			continue // wrong channel secret; try the next mapping
		}
		dec, channelHash, found = d, m.channelHash, true
		break
	}
	if !found {
		wireHash := byte(0)
		if len(lrx.Packet.Payload) > 0 {
			wireHash = lrx.Packet.Payload[0]
		}
		b.debugf("meshcore: GRP_TXT packet (wire channel_hash %#x, %d-byte payload) didn't decrypt against any configured meshcore.hashtag/secret_hex/public — check it matches the sender's actual channel", wireHash, len(lrx.Packet.Payload))
		return
	}

	b.deliverMeshcoreChannelText("raw log", channelHash, dec.TimestampUnix, dec.Text, &lrx.Packet)
}

// handleMeshcoreChannelMessage is called for every message retrieved via
// the device's own CMD_SYNC_NEXT_MESSAGE queue (see
// meshcore.Session.ChannelMessages) — a second, independent inbound path
// alongside handleMeshcorePacket's raw-log decrypt: the device already
// decrypted this using the secret registered (via RegisterChannel, in
// registerMeshcoreChannels) at msg.SlotIndex. Resolves that slot back to
// our own channelHash via hashBySlot; a slot we don't recognise (registered
// by something else, or a stale mapping from before a reconnect) is
// silently ignored.
func (b *Bridge) handleMeshcoreChannelMessage(msg meshcore.ChannelMessage) {
	channelHash, ok := b.hashBySlot[msg.SlotIndex]
	if !ok {
		return
	}
	// The sync path carries no path/hop/scope metadata (see
	// registerMeshcoreChannels' doc comment) — nil here means neither
	// per-mapping rx_scopes filtering nor repeater-path logging apply to
	// this delivery.
	b.deliverMeshcoreChannelText("sync", channelHash, msg.TimestampUnix, msg.Text, nil)
}

// deliverMeshcoreChannelText is the shared tail for both meshcore inbound
// paths: given a decoded (channelHash, timestampUnix, text) tuple — however
// it was obtained — it suppresses our own echo/duplicate deliveries once,
// then reposts to every bridge sharing that channel's Discord webhook (if
// it has one) and relays to every such bridge's Meshtastic channel (if it
// has one and isn't read-only) — each distinct physical Meshtastic channel
// is only sent to once, even if more than one sibling bridge points at it.
// Sharing this tail across both paths is what makes them naturally
// deduplicate against each other: whichever path delivers a message first
// wins, and the other's later delivery of the same packet is a harmless
// dedup hit.
//
// pkt is the raw-log path's parsed packet (nil from the sync path, which
// carries no route/hop metadata at all — see handleMeshcoreChannelMessage).
// It's used for two things, both skipped when nil: naming which repeater(s)
// relayed a self-echo back to us when meshcore.retry_on_no_repeat is enabled
// (see formatRepeaterPath), and evaluating each sibling mapping's OWN
// rx_scopes override against this specific packet's scope (each sibling
// checked independently in the fan-out loop below — deliberately not
// resolved once up front, since two sibling bridges sharing this channel
// may configure different rx_scopes, and this is the only point where every
// mapping sharing channelHash is visited).
func (b *Bridge) deliverMeshcoreChannelText(source string, channelHash byte, timestampUnix uint32, text string, pkt *meshcore.Packet) {
	echoKey := meshcoreEchoKey(channelHash, text)
	dedupKey := meshcoreDedupKey(channelHash, timestampUnix, text)
	var path []byte
	hashSize := 0
	if pkt != nil {
		path, hashSize = pkt.Path, pkt.HashSize
	}
	if outcome := b.consumeSelfEcho(echoKey, lastHopHash(path, hashSize)); outcome != notSelfEcho {
		if outcome == selfEchoIgnoredRepeater {
			b.debugf("meshcore: suppressing %q on channel_hash %#x as our own echo via an ignored repeater (relayed via %s) — not counted for retry_on_no_repeat", text, channelHash, formatRepeaterPath(path, hashSize))
		} else {
			b.debugf("meshcore: suppressing %q on channel_hash %#x as our own echo (sent it ourselves within the last %s)", text, channelHash, b.effectiveSelfEchoTTL())
			if b.meshcoreRetryOnNoRepeat {
				logf("meshcore: repeat heard for our own message on channel_hash %#x, relayed via %s", channelHash, formatRepeaterPath(path, hashSize))
			}
		}
		// Mark the dedup key too: with two independent inbound paths (raw
		// log + sync), the same physical echo can be delivered twice. This
		// consume only fires for the first; without marking dedup here, the
		// second delivery would find neither map populated and slip through
		// as if it were a brand new message.
		b.isDuplicateInbound(dedupKey)
		return
	}
	if b.isDuplicateInbound(dedupKey) {
		b.debugf("meshcore: suppressing %q on channel_hash %#x as a duplicate delivery (already relayed this exact packet within the last %s)", text, channelHash, inboundDedupTTL)
		return
	}

	sender, body := splitSenderText(text)
	b.debugf("meshcore: received via %s on channel_hash %#x from %q: %q", source, channelHash, sender, body)

	session := b.meshtasticSessionRef()
	relayedIdx := map[uint32]bool{}
	for _, m := range b.byName {
		if !m.meshcoreEnabled || m.channelHash != channelHash {
			continue
		}
		if pkt != nil && len(m.rxScopes) > 0 {
			matches, err := pkt.MatchesAnyScope(m.rxScopes)
			if err != nil {
				b.debugf("meshcore: bridge %q: error checking rx_scopes: %v", m.cfg.Name, err)
				continue
			}
			if !matches {
				b.debugf("meshcore: bridge %q: dropping message on channel_hash %#x — its scope doesn't match configured rx_scopes %v", m.cfg.Name, channelHash, m.rxScopes)
				continue
			}
		}
		tag := formatSenderName(sender, originMeshcore, m.senderFormat)
		if m.discordEnabled {
			b.postToWebhook(m, sender, tag, body)
		}
		if m.meshtasticEnabled {
			skip := false
			if session != nil {
				if idx, ok := session.ResolveChannelIndex(m.cfg.Meshtastic.ChannelName); ok {
					skip = relayedIdx[idx]
					relayedIdx[idx] = true
				}
			}
			if !skip {
				b.sendMeshtastic(m, tag, body)
			}
		}
	}
}

// handleMeshtasticMessage is called for every TEXT_MESSAGE_APP packet the
// Meshtastic device hears. Unlike MeshCore, sender identity is
// protocol-native (msg.FromName, resolved via session's node DB) — never
// text-parsed. As with handleMeshcorePacket, multiple bridges may share the
// same Meshtastic channel (relaying it into different Discord guilds), so
// the message is reposted to every matching bridge's Discord webhook and
// relayed to every matching bridge's MeshCore channel (if enabled and not
// read-only) — each distinct MeshCore channel is only sent to once.
func (b *Bridge) handleMeshtasticMessage(session *meshtastic.Session, msg meshtastic.TextMessage) {
	var matches []*mapping
	for _, m := range b.byName {
		if !m.meshtasticEnabled {
			continue
		}
		idx, ok := session.ResolveChannelIndex(m.cfg.Meshtastic.ChannelName)
		if !ok {
			// A genuine misconfiguration (the bridge's channel_name isn't a
			// slot on the device) — worth surfacing, but only once, since it
			// would otherwise fire on every inbound message.
			if b.warnMeshtasticChanOnce(m.cfg.Meshtastic.ChannelName) {
				logf("bridge %q: meshtastic channel %q is not configured on the attached device (dropping inbound messages for it until fixed)", m.cfg.Name, m.cfg.Meshtastic.ChannelName)
			}
			continue
		}
		if idx == msg.ChannelIndex {
			matches = append(matches, m)
		}
	}
	if len(matches) == 0 {
		// Expected on a busy mesh: the device hears every channel it's
		// configured for, most of which this bridge doesn't relay. Debug-only,
		// matching the MeshCore raw-log path's "no channel matched" handling.
		b.debugf("meshtastic: no bridge matches channel index %d for message from %s", msg.ChannelIndex, msg.FromName)
		return
	}

	echoKey := meshtasticEchoKey(msg.ChannelIndex, msg.Text)
	dedupKey := meshtasticDedupKey(msg.ChannelIndex, msg.PacketID, msg.Text)
	// repeaterHash is always nil here: Meshtastic carries no per-hop relay
	// identity (see the doc comment below), so meshcore.ignore_repeat_from's
	// equivalent can never apply — every pending echo counts as heard.
	if b.consumeSelfEcho(echoKey, nil) != notSelfEcho {
		b.debugf("meshtastic: suppressing %q on channel index %d as our own echo (sent it ourselves within the last %s)", msg.Text, msg.ChannelIndex, b.effectiveSelfEchoTTL())
		if b.meshtasticRetryOnNoRepeat {
			// Unlike MeshCore's hop path, the Meshtastic client API exposes no
			// per-hop relay identity for a rebroadcast packet — msg.From is
			// always the original sender, never carried by hop, so we can only
			// confirm a repeat was heard, not name which node rebroadcast it.
			logf("meshtastic: repeat heard for our own message on channel index %d (Meshtastic doesn't expose which node rebroadcast it)", msg.ChannelIndex)
		}
		// Mark the dedup key too: a repeated flood-hop delivery of this same
		// echo would otherwise find neither map populated (this consume only
		// fires once) and slip through as if it were a brand new message.
		b.isDuplicateInbound(dedupKey)
		return
	}
	if b.isDuplicateInbound(dedupKey) {
		b.debugf("meshtastic: suppressing %q on channel index %d as a duplicate delivery (already relayed this exact packet within the last %s)", msg.Text, msg.ChannelIndex, inboundDedupTTL)
		return
	}

	b.debugf("meshtastic: received on channel index %d from %s: %q", msg.ChannelIndex, msg.FromName, msg.Text)

	relayedHash := map[byte]bool{}
	for _, m := range matches {
		tag := formatSenderName(msg.FromName, originMeshtastic, m.senderFormat)
		if m.discordEnabled {
			b.postToWebhook(m, msg.FromName, tag, msg.Text)
		}
		if m.meshcoreEnabled && !relayedHash[m.channelHash] {
			relayedHash[m.channelHash] = true
			b.sendMeshcore(m, tag, msg.Text)
		}
	}
}

// postToWebhook reposts body to m's Discord webhook under displaySender
// (which may carry a sender_format origin tag), with an avatar generated
// from rawSender (the untagged name, so a person's avatar stays stable
// regardless of sender_format). Delivery is ordered and rate-limit-aware
// (see discord.WebhookSender.Enqueue) — this call itself never blocks.
func (b *Bridge) postToWebhook(m *mapping, rawSender, displaySender, body string) {
	avatarURL := avatarURLForName(rawSender)
	m.webhook.Enqueue(displaySender, avatarURL, body)
}

// splitSenderText splits a MeshCore channel plaintext ("Name: message") into
// its sender and body. If no ": " separator is present, the whole text is
// treated as the body under a generic sender name.
func splitSenderText(text string) (sender, body string) {
	if i := strings.Index(text, ": "); i >= 0 {
		return text[:i], text[i+2:]
	}
	return "mesh", text
}

// Dedup/self-echo keys are prefixed per backend so the two protocols'
// otherwise-unrelated numeric spaces (MeshCore's byte channel_hash vs
// Meshtastic's uint32 channel index) can never collide in the shared maps.

func meshcoreDedupKey(channelHash byte, timestampUnix uint32, text string) string {
	return fmt.Sprintf("mc|%d|%d|%s", channelHash, timestampUnix, text)
}

func meshcoreEchoKey(channelHash byte, text string) string {
	return fmt.Sprintf("mc|%d|%s", channelHash, text)
}

func meshtasticDedupKey(channelIndex, packetID uint32, text string) string {
	return fmt.Sprintf("mt|%d|%d|%s", channelIndex, packetID, text)
}

func meshtasticEchoKey(channelIndex uint32, text string) string {
	return fmt.Sprintf("mt|%d|%s", channelIndex, text)
}

// formatRepeaterPath renders a MeshCore packet's hop path (each repeater's
// hashSize-byte hash, in the order each repeater appended its own hash while
// flooding the packet — see meshcore.Session's doc comment on hashSize) as a
// human-readable "hopN -> hopN-1 -> ... -> us" chain, nearest-repeater last.
// Empty/malformed path info (always the case on the sync inbound path, which
// carries no hop metadata at all) reports as such rather than an empty string.
func formatRepeaterPath(path []byte, hashSize int) string {
	if hashSize <= 0 || len(path) == 0 || len(path)%hashSize != 0 {
		return "unknown repeater (no path info for this delivery)"
	}
	hops := make([]string, 0, len(path)/hashSize)
	for i := 0; i+hashSize <= len(path); i += hashSize {
		hops = append(hops, fmt.Sprintf("%x", path[i:i+hashSize]))
	}
	return strings.Join(hops, " -> ")
}

// lastHopHash returns the most-recently-appended repeater hash in path — the
// one whose direct RF transmission our own radio received (see
// formatRepeaterPath's "nearest-repeater last" ordering) — or nil if path
// carries no complete hop (e.g. the sync path, which passes hashSize 0).
func lastHopHash(path []byte, hashSize int) []byte {
	if hashSize <= 0 || len(path) < hashSize || len(path)%hashSize != 0 {
		return nil
	}
	return path[len(path)-hashSize:]
}

// matchesAnyHash reports whether target equals any entry in hashes.
func matchesAnyHash(hashes [][]byte, target []byte) bool {
	for _, h := range hashes {
		if bytes.Equal(h, target) {
			return true
		}
	}
	return false
}

// isDuplicateInbound reports whether key was already delivered recently (a
// flooded packet arriving via another relay hop), marking it seen if not.
func (b *Bridge) isDuplicateInbound(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, seen := b.recentInbound[key]; seen {
		return true
	}
	b.recentInbound[key] = time.Now()
	return false
}

// selfEchoOutcome classifies what happened when a delivered message matched
// one of our own pending outbound sends — see consumeSelfEcho.
type selfEchoOutcome int

const (
	// notSelfEcho means key wasn't a pending outbound send at all — handle
	// the delivery as new/duplicate content.
	notSelfEcho selfEchoOutcome = iota
	// selfEchoHeard means a genuine repeat was recognised, relayed via a
	// repeater not on the ignore list (or no ignore list configured) —
	// retry_on_no_repeat's pending check (echoUnheard) will find this
	// satisfied and skip the retransmit.
	selfEchoHeard
	// selfEchoIgnoredRepeater means the echo matched, but was relayed via a
	// repeater on meshcore.ignore_repeat_from: this delivery is still
	// suppressed from being reposted as new content, but the pending entry
	// is deliberately left in place so retry_on_no_repeat's later check
	// still finds it unheard and retransmits, as if no repeat arrived at all.
	selfEchoIgnoredRepeater
)

// consumeSelfEcho reports whether key matches a message this bridge sent
// within the last effectiveSelfEchoTTL — i.e. the mesh flooding our own send
// back to us. repeaterHash, when non-nil, is the hop hash of whichever
// repeater relayed this specific delivery (see lastHopHash); if it matches
// one of the sending mapping's meshcore.ignore_repeat_from entries (recorded
// against this key by markOutboundSent), the pending entry is deliberately
// left in recentOutbound rather than removed — see selfEchoIgnoredRepeater.
// Otherwise the entry is removed so a genuine duplicate send by someone else
// later isn't silently swallowed too.
func (b *Bridge) consumeSelfEcho(key string, repeaterHash []byte) selfEchoOutcome {
	b.mu.Lock()
	defer b.mu.Unlock()
	pending, ok := b.recentOutbound[key]
	if !ok {
		return notSelfEcho
	}
	if repeaterHash != nil && matchesAnyHash(pending.ignoreRepeaters, repeaterHash) {
		return selfEchoIgnoredRepeater
	}
	delete(b.recentOutbound, key)
	return selfEchoHeard
}

// warnMeshtasticChanOnce reports whether this is the first time channelName
// has been seen as unresolvable on the attached device, marking it so
// subsequent inbound messages don't repeat the warning.
func (b *Bridge) warnMeshtasticChanOnce(channelName string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mtWarnedChan[channelName] {
		return false
	}
	b.mtWarnedChan[channelName] = true
	return true
}

// markOutboundSent records that this bridge just sent key, for
// consumeSelfEcho to recognise its own echo. ignoreRepeaters is nil for
// Meshtastic sends (no repeater identity available at all); for MeshCore
// sends it's the originating mapping's resolved meshcore.ignore_repeat_from
// list, variadic purely so existing single-argument call sites (tests, and
// anywhere the ignore list doesn't apply) keep compiling unchanged.
func (b *Bridge) markOutboundSent(key string, ignoreRepeaters ...[]byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recentOutbound[key] = pendingEcho{sentAt: time.Now(), ignoreRepeaters: ignoreRepeaters}
}

// echoUnheard reports whether key is still pending in recentOutbound — i.e.
// consumeSelfEcho hasn't observed a non-ignored repeat of it yet — and
// removes it either way, so a genuine echo arriving after this check fires
// isn't mistaken for an echo of the retransmit that this triggers. Used by
// meshcore.retry_on_no_repeat (see transmitMeshcore) and meshtastic.
// retry_on_no_repeat (see transmitMeshtastic); effectiveSelfEchoTTL keeps the
// sweep from ever purging a key before the configured retry_wait_ms elapses.
func (b *Bridge) echoUnheard(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, pending := b.recentOutbound[key]
	delete(b.recentOutbound, key)
	return pending
}
