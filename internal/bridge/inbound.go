package bridge

import (
	"fmt"
	"strings"
	"time"

	"github.com/hectospark/hoplink/internal/meshcore"
	"github.com/hectospark/hoplink/internal/meshtastic"
)

// handleMeshcorePacket is called for every RF packet the MeshCore radio
// hears. It tries to decrypt GRP_TXT payloads against each meshcore-enabled
// bridge's secret; a successful decrypt identifies the channel itself
// (channelHash), not a single "owning" bridge, since multiple bridges may
// share the same MeshCore channel (e.g. relaying one mesh channel into two
// different Discord guilds). The message is then reposted to every such
// bridge's Discord webhook (if it has one) and relayed to every such
// bridge's Meshtastic channel (if it has one and isn't read-only) — each
// distinct physical Meshtastic channel is only sent to once, even if more
// than one sibling bridge points at it.
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
		return
	}

	echoKey := meshcoreEchoKey(channelHash, dec.Text)
	if b.consumeSelfEcho(echoKey) {
		return
	}
	dedupKey := meshcoreDedupKey(channelHash, dec.TimestampUnix, dec.Text)
	if b.isDuplicateInbound(dedupKey) {
		return
	}

	sender, body := splitSenderText(dec.Text)
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
	if b.consumeSelfEcho(echoKey) {
		return
	}
	dedupKey := meshtasticDedupKey(msg.ChannelIndex, msg.PacketID, msg.Text)
	if b.isDuplicateInbound(dedupKey) {
		return
	}

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
