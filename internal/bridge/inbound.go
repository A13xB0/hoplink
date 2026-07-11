package bridge

import (
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
// Discord guilds).
func (b *Bridge) handleMeshcorePacket(lrx meshcore.LogRxData) {
	if lrx.Packet.PayloadType != meshcore.PayloadTypeGrpTxt {
		return
	}

	var dec meshcore.GroupTextDecrypt
	var channelHash byte
	var rxScopes []string
	found := false
	for _, m := range b.byName {
		if !m.meshcoreEnabled {
			continue
		}
		d, ok := meshcore.DecryptGroupText(m.secret, lrx.Packet.Payload)
		if !ok {
			continue // wrong channel secret; try the next mapping
		}
		dec, channelHash, rxScopes, found = d, m.channelHash, m.rxScopes, true
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

	// rx_scopes filtering only applies here, on the raw-log path: this is
	// the only place a packet's transport code (and thus its flood scope)
	// is visible at all — the sync path's PACKET_CHANNEL_MSG_RECV carries no
	// route/transport-code metadata to filter on.
	if len(rxScopes) > 0 {
		matches, err := lrx.Packet.MatchesAnyScope(rxScopes)
		if err != nil {
			b.debugf("meshcore: error checking rx_scopes for channel_hash %#x: %v", channelHash, err)
			return
		}
		if !matches {
			b.debugf("meshcore: dropping GRP_TXT packet on channel_hash %#x — its scope doesn't match configured rx_scopes %v", channelHash, rxScopes)
			return
		}
	}

	b.deliverMeshcoreChannelText("raw log", channelHash, dec.TimestampUnix, dec.Text)
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
	b.deliverMeshcoreChannelText("sync", channelHash, msg.TimestampUnix, msg.Text)
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
func (b *Bridge) deliverMeshcoreChannelText(source string, channelHash byte, timestampUnix uint32, text string) {
	echoKey := meshcoreEchoKey(channelHash, text)
	dedupKey := meshcoreDedupKey(channelHash, timestampUnix, text)
	if b.consumeSelfEcho(echoKey) {
		b.debugf("meshcore: suppressing %q on channel_hash %#x as our own echo (sent it ourselves within the last %s)", text, channelHash, selfEchoTTL)
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
			logf("bridge %q: meshtastic channel %q is not configured on the attached device (dropping inbound message)", m.cfg.Name, m.cfg.Meshtastic.ChannelName)
			continue
		}
		if idx == msg.ChannelIndex {
			matches = append(matches, m)
		}
	}
	if len(matches) == 0 {
		logf("meshtastic: no bridge matches channel index %d for message from %s (check meshtastic.channel_name against the attached device's actual channel slots)", msg.ChannelIndex, msg.FromName)
		return
	}

	echoKey := meshtasticEchoKey(msg.ChannelIndex, msg.Text)
	dedupKey := meshtasticDedupKey(msg.ChannelIndex, msg.PacketID, msg.Text)
	if b.consumeSelfEcho(echoKey) {
		b.debugf("meshtastic: suppressing %q on channel index %d as our own echo (sent it ourselves within the last %s)", msg.Text, msg.ChannelIndex, selfEchoTTL)
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

// consumeSelfEcho reports whether key matches a message this bridge sent
// within the last selfEchoTTL — i.e. the mesh flooding our own send back to
// us — and if so removes it so a genuine duplicate send by someone else
// later isn't silently swallowed too.
func (b *Bridge) consumeSelfEcho(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.recentOutbound[key]; ok {
		delete(b.recentOutbound, key)
		return true
	}
	return false
}

// markOutboundSent records that this bridge just sent key, for
// consumeSelfEcho to recognise its own echo.
func (b *Bridge) markOutboundSent(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recentOutbound[key] = time.Now()
}

// echoUnheard reports whether key is still pending in recentOutbound — i.e.
// consumeSelfEcho hasn't observed a repeat of it yet — and removes it either
// way, so a genuine echo arriving after this check fires isn't mistaken for
// an echo of the retransmit that this triggers. Used only by
// meshcore.retry_on_no_repeat (see transmitMeshcore); repeatRetryWait is kept
// below selfEchoTTL so the periodic sweep can never race this check by
// purging the key first.
func (b *Bridge) echoUnheard(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, pending := b.recentOutbound[key]
	delete(b.recentOutbound, key)
	return pending
}
