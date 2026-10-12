package bridge

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hectospark/hoplink/internal/discord"
	"github.com/hectospark/hoplink/internal/meshcore"
	"github.com/hectospark/hoplink/internal/meshtastic"
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
		// Typically an attachment-only or embed-only post: there's no text
		// to put on the mesh. Debug-visible rather than fully silent.
		b.debugDiscord("RX bridge %q: dropping message with no text content from %s", m.cfg.Name, msg.AuthorName)
		return
	}
	b.debugDiscord("RX bridge %q: from %s: %q", m.cfg.Name, msg.AuthorName, msg.Content)
	if m.cfg.GuildID != "" && msg.GuildID != "" && m.cfg.GuildID != msg.GuildID {
		logfDiscord("RX bridge %q: ignoring message from guild %q (configured for guild %q)", m.cfg.Name, msg.GuildID, m.cfg.GuildID)
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
// in range, a small mesh with no repeater, etc.), so it's immediate and reliable, and it
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
	chunks := composeChunks(name, content, meshcoreMaxChunkBytes)
	for i, chunk := range chunks {
		if i > 0 && b.meshcoreChunkDelay > 0 {
			time.Sleep(b.meshcoreChunkDelay)
		}
		b.transmitMeshcore(m, chunk, uint32(time.Now().Unix()), i+1, len(chunks), 0)
	}
}

// transmitMeshcore sends chunk once, stamped with tsUnix (the caller
// generates it for the original send; a retry reuses it so the retransmit is
// byte-identical over RF — mesh-side dedup and sibling bridge instances then
// treat it as the same message, not a new one). If meshcore.
// retry_on_no_repeat is enabled and this isn't already a retry, it schedules
// a check meshcoreRetryWait later: if no self-echo of chunk was heard by
// then (see echoUnheard) — which also covers an echo heard only via a
// repeater on meshcore.ignore_repeat_from, since consumeSelfEcho
// deliberately leaves that case pending — nothing repeated the message
// across the mesh, so it's retransmitted once more. attempt is 0 for the
// original send, 1 for the single allowed retry — purely to stop it
// recursing past maxRepeatRetries.
//
// (There is no protocol-level delivery signal to use instead: GRP_TXT flood
// packets carry no ack — PUSH_CODE_SEND_CONFIRMED only fires for direct
// messages — so listening for our own packet re-flooded genuinely is the
// best available confirmation.)
func (b *Bridge) transmitMeshcore(m *mapping, chunk string, tsUnix uint32, chunkIdx, chunkTotal, attempt int) {
	session := b.meshcoreSessionRef()
	if session == nil {
		logfMeshcore("TX bridge %q: not connected; dropping outgoing message", m.cfg.Name)
		return
	}
	pkt, err := meshcore.BuildChannelPacket(m.secret, b.hashSize, m.scopeKey, chunk, tsUnix)
	if err != nil {
		logfMeshcore("TX bridge %q: building packet failed (chunk %d/%d): %v", m.cfg.Name, chunkIdx, chunkTotal, err)
		return
	}
	pktHash := pkt.Hash()
	echoKey := meshcoreEchoKey(m.channelHash, chunk)
	// Listen-before-talk: wait for quiet air (and our own min TX spacing)
	// before hitting the channel — bounded, so a message is never held
	// hostage. Retries get a longer cap: they're already late, and
	// retransmitting into the congestion that swallowed the original copy
	// helps nobody.
	deferCap := b.busyMaxDefer
	if attempt > 0 {
		deferCap += 2 * time.Second
	}
	b.waitForQuietAir(deferCap, fmt.Sprintf("bridge %q chunk %d/%d", m.cfg.Name, chunkIdx, chunkTotal))
	b.markOwnPacket(meshcoreDedupKey(m.channelHash, tsUnix, chunk))
	b.markOutboundSent(echoKey, m.ignoreRepeatFrom...)
	b.trackDelivery(pktHash, m, chunkIdx, chunkTotal)
	err = b.withTxGuard(func() error {
		return session.SendPacket(pkt)
	})
	if err != nil {
		b.unmarkOutboundSent(echoKey)
		logfMeshcore("TX bridge %q: send failed (chunk %d/%d, attempt %d/%d): %v", m.cfg.Name, chunkIdx, chunkTotal, attempt+1, b.mcMaxRetries()+1, err)
		if attempt == 0 {
			b.forgetDelivery(pktHash) // never went out: nothing to report
		} else {
			b.reportDelivery(pktHash)
		}
		return
	}
	b.noteOwnTx(len(chunk))
	logfMeshcore("TX bridge %q: sent %d bytes to channel_hash %#x (chunk %d/%d, attempt %d/%d, packet %s): radio accepted", m.cfg.Name, len(chunk), m.channelHash, chunkIdx, chunkTotal, attempt+1, b.mcMaxRetries()+1, pktHash)
	// After the wait: retransmit if retry_on_no_repeat is on and the
	// delivery rule isn't satisfied yet, otherwise the send has settled —
	// log its delivery summary. Jittered ±20% so two nodes that collided
	// once don't re-collide every retry round.
	canRetry := b.meshcoreRetryOnNoRepeat && attempt < b.mcMaxRetries()
	wait := time.Duration(float64(b.meshcoreRetryWait) * (0.8 + 0.4*rand.Float64()))
	b.afterFunc(wait, func() {
		if canRetry {
			b.echoUnheard(echoKey) // release this attempt's pending-echo slot either way
			if !b.deliveryConfirmed(pktHash) {
				logfMeshcore("TX bridge %q: delivery not confirmed within %s (needs %s), retransmitting", m.cfg.Name, b.meshcoreRetryWait, b.deliveryRule(pktHash))
				b.transmitMeshcore(m, chunk, tsUnix, chunkIdx, chunkTotal, attempt+1)
				return
			}
		}
		b.reportDelivery(pktHash)
	})
}

func (b *Bridge) sendMeshtastic(m *mapping, name, content string) {
	if m.cfg.Meshtastic.ReadOnly {
		return // this bridge's Meshtastic side only ever receives; never transmits
	}
	// Resolve once up front purely to fail fast (and log once, not per
	// chunk); transmitMeshtastic re-resolves per attempt so a retry after a
	// reconnect uses the live session and its current channel table.
	session := b.meshtasticSessionRef()
	if session == nil {
		logfMeshtastic("TX bridge %q: not connected; dropping outgoing message", m.cfg.Name)
		return
	}
	if _, ok := session.ResolveChannelIndex(m.cfg.Meshtastic.ChannelName); !ok {
		logfMeshtastic("TX bridge %q: channel %q is not configured on the attached device", m.cfg.Name, m.cfg.Meshtastic.ChannelName)
		return
	}
	chunks := composeChunks(name, content, meshtasticMaxChunkBytes)
	for i, chunk := range chunks {
		if i > 0 && b.meshtasticChunkDelay > 0 {
			time.Sleep(b.meshtasticChunkDelay)
		}
		b.transmitMeshtastic(m, chunk, meshtastic.NewPacketID(), i+1, len(chunks), 0)
	}
}

// transmitMeshtastic sends chunk once as packet pktID (the caller generates
// it for the original send; a retry reuses it so the retransmit is the same
// packet to mesh-side duplicate suppression and to sibling bridge
// instances). The live session and channel index are re-resolved on every
// attempt, so a retry scheduled before a reconnect transmits on the current
// session rather than a dead one. If meshtastic.retry_on_no_repeat is
// enabled and this isn't already a retry, it schedules a check
// meshtasticRetryWait later: if no self-echo of chunk was heard by then (see
// echoUnheard), nothing rebroadcast the message across the mesh, so it's
// retransmitted once more. attempt is 0 for the original send, 1 for the
// single allowed retry — purely to stop it recursing past maxRepeatRetries.
// Mirrors transmitMeshcore (Meshtastic has no ignore_repeat_from equivalent
// — no per-hop relay identity is available at all, see formatRepeaterPath).
func (b *Bridge) transmitMeshtastic(m *mapping, chunk string, pktID uint32, chunkIdx, chunkTotal, attempt int) {
	session := b.meshtasticSessionRef()
	if session == nil {
		logfMeshtastic("TX bridge %q: not connected; dropping outgoing message", m.cfg.Name)
		return
	}
	idx, ok := session.ResolveChannelIndex(m.cfg.Meshtastic.ChannelName)
	if !ok {
		logfMeshtastic("TX bridge %q: channel %q is not configured on the attached device", m.cfg.Name, m.cfg.Meshtastic.ChannelName)
		return
	}
	echoKey := meshtasticEchoKey(idx, chunk)
	b.markOwnPacket(meshtasticDedupKey(idx, pktID, chunk))
	b.markOutboundSent(echoKey)
	err := b.withTxGuard(func() error {
		return session.SendTextOnChannel(idx, chunk, b.meshtasticHopLimit, pktID)
	})
	if err != nil {
		b.unmarkOutboundSent(echoKey)
		logfMeshtastic("TX bridge %q: send failed on channel %q (chunk %d/%d, attempt %d/%d): %v", m.cfg.Name, m.cfg.Meshtastic.ChannelName, chunkIdx, chunkTotal, attempt+1, maxRepeatRetries+1, err)
		return
	}
	logfMeshtastic("TX bridge %q: sent %d bytes to channel %q (index %d, packet id %#08x, chunk %d/%d, attempt %d/%d): handed to radio", m.cfg.Name, len(chunk), m.cfg.Meshtastic.ChannelName, idx, pktID, chunkIdx, chunkTotal, attempt+1, maxRepeatRetries+1)
	if b.meshtasticRetryOnNoRepeat && attempt < maxRepeatRetries {
		b.afterFunc(b.meshtasticRetryWait, func() {
			if b.echoUnheard(echoKey) {
				logfMeshtastic("TX bridge %q: no repeat heard within %s, retransmitting", m.cfg.Name, b.meshtasticRetryWait)
				b.transmitMeshtastic(m, chunk, pktID, chunkIdx, chunkTotal, attempt+1)
			}
		})
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

	// Pathological case (e.g. an extremely long name, or a per-bridge
	// max_message_bytes far above what 50 chunks can carry):
	// hard-truncate to one message — loudly, never silently.
	prefixLen := len(name) + 2 // "Name: "
	avail := maxBytes - prefixLen
	if avail < 0 {
		avail = 0
	}
	logf("message from %q too long to chunk (would need more than 50 chunks of %d bytes); hard-truncating to a single chunk — the rest is dropped", name, maxBytes)
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
