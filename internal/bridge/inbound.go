package bridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hectospark/hoplink/internal/meshcore"
	"github.com/hectospark/hoplink/internal/meshtastic"
)

// handleMeshcorePacket is called for every RF packet the MeshCore radio
// hears. It tries to decrypt GRP_TXT payloads against each meshcore-enabled
// bridge's secret and, on a match, reposts the message to that bridge's
// Discord webhook (if it has one) and, if the bridge also has Meshtastic
// enabled, relays it there too.
func (b *Bridge) handleMeshcorePacket(lrx meshcore.LogRxData) {
	if lrx.Packet.PayloadType != meshcore.PayloadTypeGrpTxt {
		return
	}

	for _, m := range b.byName {
		if !m.meshcoreEnabled {
			continue
		}
		dec, ok := meshcore.DecryptGroupText(m.secret, lrx.Packet.Payload)
		if !ok {
			continue // wrong channel secret; try the next mapping
		}

		echoKey := meshcoreEchoKey(m.channelHash, dec.Text)
		if b.consumeSelfEcho(echoKey) {
			return
		}
		dedupKey := meshcoreDedupKey(m.channelHash, dec.TimestampUnix, dec.Text)
		if b.isDuplicateInbound(dedupKey) {
			return
		}

		sender, body := splitSenderText(dec.Text)
		if m.discordEnabled {
			b.postToWebhook(m, sender, formatSenderName(sender, originMeshcore, m.senderFormat), body)
		}
		if m.meshtasticEnabled {
			b.sendMeshtastic(m, formatSenderName(sender, originMeshcore, m.senderFormat), body)
		}
		return // channel hashes/secrets are effectively unique; stop after first match
	}
}

// handleMeshtasticMessage is called for every TEXT_MESSAGE_APP packet the
// Meshtastic device hears. Unlike MeshCore, sender identity is
// protocol-native (msg.FromName, resolved via session's node DB) — never
// text-parsed. On a match, reposts to Discord (if the bridge has one) and,
// if the bridge also has MeshCore enabled, relays it there too.
func (b *Bridge) handleMeshtasticMessage(session *meshtastic.Session, msg meshtastic.TextMessage) {
	for _, m := range b.byName {
		if !m.meshtasticEnabled {
			continue
		}
		idx, ok := session.ResolveChannelIndex(m.cfg.Meshtastic.ChannelName)
		if !ok {
			logf("bridge %q: meshtastic channel %q is not configured on the attached device (dropping inbound message)", m.cfg.Name, m.cfg.Meshtastic.ChannelName)
			continue
		}
		if idx != msg.ChannelIndex {
			continue
		}

		echoKey := meshtasticEchoKey(msg.ChannelIndex, msg.Text)
		if b.consumeSelfEcho(echoKey) {
			return
		}
		dedupKey := meshtasticDedupKey(msg.ChannelIndex, msg.PacketID, msg.Text)
		if b.isDuplicateInbound(dedupKey) {
			return
		}

		if m.discordEnabled {
			b.postToWebhook(m, msg.FromName, formatSenderName(msg.FromName, originMeshtastic, m.senderFormat), msg.Text)
		}
		if m.meshcoreEnabled {
			b.sendMeshcore(m, formatSenderName(msg.FromName, originMeshtastic, m.senderFormat), msg.Text)
		}
		return // one bridge per (name, channel); stop after first match
	}
	logf("meshtastic: no bridge matches channel index %d for message from %s (check meshtastic.channel_name against the attached device's actual channel slots)", msg.ChannelIndex, msg.FromName)
}

// postToWebhook reposts body to m's Discord webhook under displaySender
// (which may carry a sender_format origin tag), with an avatar generated
// from rawSender (the untagged name, so a person's avatar stays stable
// regardless of sender_format).
func (b *Bridge) postToWebhook(m *mapping, rawSender, displaySender, body string) {
	avatarURL := avatarURLForName(rawSender)
	go func(m *mapping, displaySender, avatarURL, body string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.webhook.Send(ctx, displaySender, avatarURL, body); err != nil {
			logf("posting to Discord webhook for %q: %v", m.cfg.Name, err)
		}
	}(m, displaySender, avatarURL, body)
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
