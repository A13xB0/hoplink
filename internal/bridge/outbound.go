package bridge

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hectospark/hoplink/internal/discord"
)

// meshcoreMaxChunkBytes is a conservative cap on a single MeshCore GRP_TXT
// message body (sender prefix + content). The serialised RF packet must fit
// the companion frame budget (meshcore.MaxRawPacketLen, 174 bytes): 1 header
// + 1 path_len + 3 GRP_TXT overhead (channel_hash+mac) leaves ~169 bytes for
// AES-128-ECB ciphertext, rounded down to a 160-byte (10-block) boundary,
// minus 5 bytes of timestamp+flags plaintext header = 155 usable bytes. 150
// leaves a small safety margin.
const meshcoreMaxChunkBytes = 150

// meshtasticMaxChunkBytes is a conservative cap on a single Meshtastic
// TEXT_MESSAGE_APP payload; usable text is commonly cited around 200-237
// bytes depending on LoRa preset, so 200 leaves a safety margin.
const meshtasticMaxChunkBytes = 200

// handleDiscordMessage is the discord.Bot MessageHandler: it maps the
// message's channel to a bridge (if any), enforces the guild check, the
// read-only check, and the byte-size limit, formats "<Name>: <content>",
// and sends it to whichever mesh backend(s) that bridge has enabled (each
// chunked to its own protocol's budget) as well as to any sibling bridge's
// Discord channel sharing the same MeshCore/Meshtastic channel.
func (b *Bridge) handleDiscordMessage(msg discord.IncomingMessage) {
	m, ok := b.byChan[msg.ChannelID]
	if !ok {
		return
	}
	if msg.WebhookID != "" && msg.WebhookID == m.webhook.ID() {
		return // this bridge's own webhook post, echoed back by Discord
	}
	if strings.TrimSpace(msg.Content) == "" {
		return
	}
	if m.cfg.GuildID != "" && msg.GuildID != "" && m.cfg.GuildID != msg.GuildID {
		logf("bridge %q: ignoring message from guild %q (configured for guild %q)", m.cfg.Name, msg.GuildID, m.cfg.GuildID)
		return
	}
	if m.cfg.DiscordReadOnly {
		return // this bridge's Discord side only ever receives; never relays out
	}

	senderName := formatSenderName(msg.AuthorName, originDiscord, m.senderFormat)

	length := composedLength(senderName, msg.Content)
	if length > m.maxBytes {
		b.notifyTooLong(msg, length, m.maxBytes)
		return
	}

	if m.meshcoreEnabled {
		b.sendMeshcore(m, senderName, msg.Content)
	}
	if m.meshtasticEnabled {
		b.sendMeshtastic(m, senderName, msg.Content)
	}
	b.relayDiscordToSiblings(m, msg.AuthorName, msg.Content)
}

// relayDiscordToSiblings directly reposts a Discord message to every OTHER
// bridge's Discord webhook that shares m's MeshCore channel (same secret)
// and/or Meshtastic channel (same channel_name) — e.g. two bridges relaying
// the same MeshCore hashtag channel into two different Discord guilds. This
// is a same-process software relay: it doesn't depend on the message
// actually being heard back over RF (which may never happen — no repeater
// in range, route: direct, etc.), so it's immediate and reliable, and it
// never triggers an additional mesh-side transmission beyond what m's own
// enabled backends already send above.
func (b *Bridge) relayDiscordToSiblings(m *mapping, rawSender, content string) {
	posted := map[*mapping]bool{}
	for _, o := range b.byName {
		if o == m || !o.discordEnabled || posted[o] {
			continue
		}
		sharesMeshcore := m.meshcoreEnabled && o.meshcoreEnabled && o.channelHash == m.channelHash
		sharesMeshtastic := m.meshtasticEnabled && o.meshtasticEnabled &&
			strings.EqualFold(o.cfg.Meshtastic.ChannelName, m.cfg.Meshtastic.ChannelName)
		if sharesMeshcore || sharesMeshtastic {
			posted[o] = true
			b.postToWebhook(o, rawSender, formatSenderName(rawSender, originDiscord, o.senderFormat), content)
		}
	}
}

func (b *Bridge) sendMeshcore(m *mapping, name, content string) {
	if m.cfg.MeshCore.ReadOnly {
		return // this bridge's MeshCore side only ever receives; never transmits
	}
	session := b.meshcoreSessionRef()
	if session == nil {
		logf("bridge %q: meshcore not currently connected; dropping outgoing message", m.cfg.Name)
		return
	}
	for _, chunk := range composeChunks(name, content, meshcoreMaxChunkBytes) {
		b.markOutboundSent(meshcoreEchoKey(m.channelHash, chunk))
		err := b.withTxGuard(func() error {
			return session.SendChannelMessage(m.secret, b.route, b.hashSize, m.scopeKey, chunk)
		})
		if err != nil {
			logf("sending to meshcore channel %q: %v", m.cfg.Name, err)
		}
	}
}

func (b *Bridge) sendMeshtastic(m *mapping, name, content string) {
	if m.cfg.Meshtastic.ReadOnly {
		return // this bridge's Meshtastic side only ever receives; never transmits
	}
	session := b.meshtasticSessionRef()
	if session == nil {
		logf("bridge %q: meshtastic not currently connected; dropping outgoing message", m.cfg.Name)
		return
	}
	idx, ok := session.ResolveChannelIndex(m.cfg.Meshtastic.ChannelName)
	if !ok {
		logf("bridge %q: meshtastic channel %q is not configured on the attached device", m.cfg.Name, m.cfg.Meshtastic.ChannelName)
		return
	}
	for _, chunk := range composeChunks(name, content, meshtasticMaxChunkBytes) {
		b.markOutboundSent(meshtasticEchoKey(idx, chunk))
		err := b.withTxGuard(func() error {
			return session.SendText(m.cfg.Meshtastic.ChannelName, chunk)
		})
		if err != nil {
			logf("sending to meshtastic channel %q: %v", m.cfg.Name, err)
		}
	}
}

// composeChunks formats "<name>: <content>" and, if it exceeds maxBytes,
// splits content across multiple messages each carrying a "(i/n)"
// indicator, never splitting a UTF-8 rune across chunks.
func composeChunks(name, content string, maxBytes int) []string {
	full := name + ": " + content
	if len(full) <= maxBytes {
		return []string{full}
	}

	contentRunes := []rune(content)
	for n := 2; n <= 50; n++ {
		prefixLen := len(fmt.Sprintf("%s (%d/%d): ", name, n, n))
		avail := maxBytes - prefixLen
		if avail <= 0 {
			continue
		}
		pieces := splitRunesByByteLen(contentRunes, avail)
		if len(pieces) <= n {
			out := make([]string, len(pieces))
			for i, p := range pieces {
				out[i] = fmt.Sprintf("%s (%d/%d): %s", name, i+1, len(pieces), p)
			}
			return out
		}
	}

	// Pathological case (e.g. an extremely long name): hard-truncate to one message.
	prefixLen := len(name) + 2 // "Name: "
	avail := maxBytes - prefixLen
	if avail < 0 {
		avail = 0
	}
	return []string{fmt.Sprintf("%s: %s", name, truncateRunes(contentRunes, avail))}
}

func splitRunesByByteLen(runes []rune, maxBytes int) []string {
	var chunks []string
	var buf []rune
	bufLen := 0
	for _, r := range runes {
		rl := utf8.RuneLen(r)
		if bufLen+rl > maxBytes && bufLen > 0 {
			chunks = append(chunks, string(buf))
			buf = nil
			bufLen = 0
		}
		buf = append(buf, r)
		bufLen += rl
	}
	if bufLen > 0 || len(chunks) == 0 {
		chunks = append(chunks, string(buf))
	}
	return chunks
}

func truncateRunes(runes []rune, maxBytes int) string {
	var buf []rune
	bufLen := 0
	for _, r := range runes {
		rl := utf8.RuneLen(r)
		if bufLen+rl > maxBytes {
			break
		}
		buf = append(buf, r)
		bufLen += rl
	}
	return string(buf)
}
