// Package bridge wires meshcore.Session and/or meshtastic.Session to an
// optional discord.Bot/WebhookSender set. Mesh channel messages are
// decrypted/decoded and, per bridge, reposted to Discord (if configured)
// under the originating node's name and/or relayed directly to the other
// mesh backend; Discord messages are composed as "<DisplayName>: <content>"
// and sent to whichever mesh backend(s) a bridge has enabled. A bridge with
// no Discord side relays purely between MeshCore and Meshtastic.
package bridge

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/hectospark/hoplink/internal/config"
	"github.com/hectospark/hoplink/internal/discord"
	"github.com/hectospark/hoplink/internal/meshcore"
	"github.com/hectospark/hoplink/internal/meshtastic"
)

// inboundDedupTTL bounds how long an inbound-message dedup key is
// remembered to suppress duplicate delivery of the same flooded packet
// arriving via multiple relay hops.
const inboundDedupTTL = 60 * time.Second

// selfEchoTTL is the floor for how long a just-sent outbound-echo key is
// remembered — both to suppress our own outbound message being re-delivered
// to Discord after the mesh floods it back to us, and (see
// effectiveSelfEchoTTL) as the minimum retry_on_no_repeat pending window.
const selfEchoTTL = 10 * time.Second

// maxRepeatRetries caps how many times a single message is retransmitted for
// never being heard repeated — one retry, not an unbounded loop.
const maxRepeatRetries = 1

// mapping is one bridge entry: whichever of MeshCore/Meshtastic/Discord it
// has enabled (at least two of the three).
type mapping struct {
	cfg          config.Bridge
	webhook      *discord.WebhookSender // nil iff !discordEnabled
	maxBytes     int                    // resolved from cfg.MaxMessageBytes or the global default
	senderFormat string                 // resolved from cfg.SenderFormat or the global default; "none"/"short"/"full"

	discordEnabled bool

	meshcoreEnabled bool
	secret          []byte   // MeshCore channel secret; valid iff meshcoreEnabled
	channelHash     byte     // valid iff meshcoreEnabled
	scopeKey        []byte   // resolved from cfg.MeshCore.FloodScope or the global default; nil for unscoped ROUTE_TYPE_FLOOD; valid iff meshcoreEnabled
	rxScopes        []string // resolved from cfg.MeshCore.RxScopes or the global default; empty = accept every scope on the raw-log path; valid iff meshcoreEnabled
	// ignoreRepeatFrom is the decoded (resolved from cfg.MeshCore.
	// IgnoreRepeatFrom or the global default) list of repeater hop hashes
	// whose relay of one of our own sends from this mapping should not
	// count as a heard repeat for retry_on_no_repeat — see
	// meshcore.ignore_repeat_from and consumeSelfEcho. Valid iff
	// meshcoreEnabled; empty means every repeater's relay counts.
	ignoreRepeatFrom [][]byte

	meshtasticEnabled bool // channel resolution happens live against whichever meshtastic.Session is attached (see outbound.go/inbound.go)
}

// notifier is the subset of *discord.Bot the bridge needs for
// oversized-message notices — kept as an interface (rather than referencing
// *discord.Bot directly) so tests can substitute a fake instead of making
// real Discord API calls.
type notifier interface {
	Reply(channelID, messageID, content string) error
}

// Bridge orchestrates the mesh <-> Discord relay for a set of mappings. A
// single Bridge can have a MeshCore session, a Meshtastic session, both, or
// (temporarily, between reconnects) neither attached; RunMeshcore/
// RunMeshtastic each own one backend's connected lifetime independently, so
// one backend reconnecting never disturbs the other.
type Bridge struct {
	notify                    notifier
	hashSize                  int    // path hash bytes/hop for our outgoing MeshCore packets (1-3)
	meshtasticHopLimit        uint32 // meshtastic.hop_limit — how many times other nodes may rebroadcast our outgoing packets (0-7)
	debug                     bool
	meshcoreRetryOnNoRepeat   bool          // meshcore.retry_on_no_repeat — see maxRepeatRetries
	meshtasticRetryOnNoRepeat bool          // meshtastic.retry_on_no_repeat — mirrors meshcoreRetryOnNoRepeat
	meshcoreChunkDelay        time.Duration // meshcore.chunk_delay_ms — extra pause between chunks of a split message
	meshtasticChunkDelay      time.Duration // meshtastic.chunk_delay_ms — extra pause between chunks of a split message
	meshcoreRetryWait         time.Duration // meshcore.retry_wait_ms — how long transmitMeshcore waits for a repeat before retrying
	meshtasticRetryWait       time.Duration // meshtastic.retry_wait_ms — how long transmitMeshtastic waits for a repeat before retrying
	byName                    []*mapping
	byChan                    map[string]*mapping // Discord channel ID -> mapping

	// hashBySlot maps a device channel slot index (registered via
	// meshcore.Session.RegisterChannel) back to our own channelHash, so
	// handleMeshcoreChannelMessage (the sync/CMD_SYNC_NEXT_MESSAGE path) can
	// resolve which bridge(s) a synced message belongs to. Rebuilt once per
	// RunMeshcore(session) attach; only ever touched by that goroutine, so
	// it needs no separate lock.
	hashBySlot map[byte]byte

	sessionMu         sync.Mutex
	meshcoreSession   *meshcore.Session
	meshtasticSession *meshtastic.Session

	// txGuard serialises outbound MeshCore and Meshtastic sends across every
	// bridge so this process never asks both radios to transmit at the same
	// instant — a best-effort RF interference mitigation for a co-located
	// MeshCore radio + Meshtastic device (config: coexistence.*). It cannot
	// guarantee non-overlapping airtime, only non-overlapping send calls.
	txGuardEnabled bool
	txGuardGap     time.Duration
	txMu           sync.Mutex

	mu             sync.Mutex
	recentInbound  map[string]time.Time
	recentOutbound map[string]pendingEcho
	// mtWarnedChan tracks meshtastic channel_names we've already warned about
	// being absent from the attached device, so a genuinely-misconfigured
	// bridge is surfaced once rather than on every inbound message. Guarded by
	// mu. Keyed by channel_name (the config value that failed to resolve).
	mtWarnedChan map[string]bool
}

// pendingEcho tracks one outbound send awaiting either self-echo suppression
// (don't repost our own relayed message as if it were new) or, when that
// protocol's retry_on_no_repeat is enabled, a decision on whether to
// retransmit — see consumeSelfEcho/echoUnheard.
type pendingEcho struct {
	sentAt time.Time
	// ignoreRepeaters lists repeater hop hashes (meshcore only; always nil
	// for Meshtastic sends, which carry no repeater identity at all — see
	// formatRepeaterPath) whose relay of this message should not count as a
	// heard repeat for retry_on_no_repeat purposes (see mapping.
	// ignoreRepeatFrom / meshcore.ignore_repeat_from). The message is still
	// suppressed from being reposted as new content either way.
	ignoreRepeaters [][]byte
}

// withTxGuard runs send while holding the shared TX lock (if
// coexistence.avoid_simultaneous_tx is enabled), then pauses for the
// configured gap before releasing it. With the guard disabled, send just
// runs directly.
func (b *Bridge) withTxGuard(send func() error) error {
	if !b.txGuardEnabled {
		return send()
	}
	b.txMu.Lock()
	defer b.txMu.Unlock()
	err := send()
	if b.txGuardGap > 0 {
		time.Sleep(b.txGuardGap)
	}
	return err
}

// New builds a Bridge for cfg's bridge mappings and, if bot is non-nil,
// wires its message handler. bot is nil when no bridge has a Discord side
// configured (config.Config.DiscordEnabled() is false) — hoplink then runs
// as a pure MeshCore<->Meshtastic relay with no Discord gateway connection.
// New does not connect to either mesh backend — attach sessions via
// RunMeshcore/RunMeshtastic (each may be called repeatedly across
// reconnects; the caller, cmd/hoplink, owns that lifecycle).
func New(cfg *config.Config, bot *discord.Bot) (*Bridge, error) {
	b := &Bridge{
		hashSize:                  cfg.Meshcore.PathHashBytes,
		meshtasticHopLimit:        uint32(cfg.Meshtastic.ResolvedHopLimit()),
		debug:                     cfg.Debug,
		meshcoreRetryOnNoRepeat:   cfg.Meshcore.RetryOnNoRepeat,
		meshtasticRetryOnNoRepeat: cfg.Meshtastic.RetryOnNoRepeat,
		meshcoreChunkDelay:        cfg.Meshcore.ChunkDelay(),
		meshtasticChunkDelay:      cfg.Meshtastic.ChunkDelay(),
		meshcoreRetryWait:         cfg.Meshcore.RetryWait(),
		meshtasticRetryWait:       cfg.Meshtastic.RetryWait(),
		txGuardEnabled:            cfg.Coexistence.Enabled(),
		txGuardGap:                cfg.Coexistence.GapDuration(),
		byChan:                    make(map[string]*mapping),
		recentInbound:             make(map[string]time.Time),
		recentOutbound:            make(map[string]pendingEcho),
		mtWarnedChan:              make(map[string]bool),
	}

	for _, bc := range cfg.Bridges {
		if !bc.IsEnabled() {
			logf("bridge %q: disabled, skipping", bc.Name)
			continue
		}
		discordEnabled := bc.DiscordChannelID != ""
		m := &mapping{
			cfg:               bc,
			maxBytes:          bc.ResolvedMaxMessageBytes(cfg.Limits.MaxMessageBytes),
			senderFormat:      bc.ResolvedSenderFormat(cfg.SenderFormat),
			discordEnabled:    discordEnabled,
			meshcoreEnabled:   bc.MeshCore.Enabled,
			meshtasticEnabled: bc.Meshtastic.Enabled,
		}
		if discordEnabled {
			m.webhook = discord.NewWebhookSender(bc.DiscordWebhookURL, bc.Name)
		}
		if bc.MeshCore.Enabled {
			secret, err := bc.Secret()
			if err != nil {
				return nil, err
			}
			chHash, err := meshcore.ChannelHash(secret)
			if err != nil {
				return nil, fmt.Errorf("bridge %q: %w", bc.Name, err)
			}
			m.secret = secret
			m.channelHash = chHash
			m.scopeKey = bc.ResolvedScopeKey(cfg.Meshcore.FloodScope)
			m.rxScopes = bc.ResolvedRxScopes(cfg.Meshcore.RxScopes)
			for _, h := range bc.ResolvedIgnoreRepeatFrom(cfg.Meshcore.IgnoreRepeatFrom) {
				decoded, err := hex.DecodeString(h)
				if err != nil {
					return nil, fmt.Errorf("bridge %q: meshcore.ignore_repeat_from %q: %w", bc.Name, h, err)
				}
				m.ignoreRepeatFrom = append(m.ignoreRepeatFrom, decoded)
			}
		}
		b.byName = append(b.byName, m)
		if discordEnabled {
			b.byChan[bc.DiscordChannelID] = m
		}
	}

	if bot != nil {
		b.notify = bot
		bot.OnMessage(b.handleDiscordMessage)
	}
	return b, nil
}

// SetMeshcoreSession attaches (or, passed nil, detaches) the currently live
// MeshCore session used for outbound sends. RunMeshcore calls this itself;
// exported for tests.
func (b *Bridge) SetMeshcoreSession(s *meshcore.Session) {
	b.sessionMu.Lock()
	b.meshcoreSession = s
	b.sessionMu.Unlock()
}

func (b *Bridge) meshcoreSessionRef() *meshcore.Session {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	return b.meshcoreSession
}

// SetMeshtasticSession attaches (or, passed nil, detaches) the currently
// live Meshtastic session used for outbound sends. RunMeshtastic calls this
// itself; exported for tests.
func (b *Bridge) SetMeshtasticSession(s *meshtastic.Session) {
	b.sessionMu.Lock()
	b.meshtasticSession = s
	b.sessionMu.Unlock()
}

func (b *Bridge) meshtasticSessionRef() *meshtastic.Session {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	return b.meshtasticSession
}

// RunMeshcore attaches session and consumes its decoded RF packets until
// ctx is cancelled or the session closes, dispatching MeshCore channel
// messages to Discord. The caller (cmd/hoplink) reconnects and calls this
// again on return; it does not affect any attached Meshtastic session.
//
// Two independent inbound paths are consumed here: LogRxFrames (raw RF log,
// decrypted ourselves — see handleMeshcorePacket) and ChannelMessages (the
// device's own CMD_SYNC_NEXT_MESSAGE queue, populated after
// registerMeshcoreChannels registers our channels on it — see
// handleMeshcoreChannelMessage). Both funnel into the same dedup/relay
// logic, so a message is only lost if both paths miss it.
func (b *Bridge) RunMeshcore(ctx context.Context, session *meshcore.Session) error {
	b.SetMeshcoreSession(session)
	defer b.SetMeshcoreSession(nil)

	b.registerMeshcoreChannels(session)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.Done():
			return session.Err()
		case lrx, ok := <-session.LogRxFrames():
			if !ok {
				return session.Err()
			}
			b.handleMeshcorePacket(lrx)
		case msg, ok := <-session.ChannelMessages():
			if !ok {
				return session.Err()
			}
			b.handleMeshcoreChannelMessage(msg)
		}
	}
}

// registerMeshcoreChannels registers every distinct configured MeshCore
// channel secret on session's attached device, so the device decodes and
// queues messages on those channels for retrieval via ChannelMessages — a
// second, independent inbound path alongside the raw log (see RunMeshcore's
// doc comment). A registration failure (e.g. the device's channel slots are
// all occupied by unrelated channels) is logged as a warning; that channel
// simply keeps working via the raw log alone — not fatal.
func (b *Bridge) registerMeshcoreChannels(session *meshcore.Session) {
	b.hashBySlot = make(map[byte]byte)
	registered := map[byte]bool{} // channelHash -> already registered this pass
	for _, m := range b.byName {
		if !m.meshcoreEnabled || registered[m.channelHash] {
			continue
		}
		registered[m.channelHash] = true

		// The public channel works differently from hashtag/private
		// channels: every MeshCore client expects it at the device's fixed
		// public slot (index 0), never searched for or claimed like the 7
		// private slots.
		if m.cfg.MeshCore.Public {
			alreadyInstalled, err := session.RegisterPublicChannel()
			if err != nil {
				logf("bridge %q: could not register the public meshcore channel for device-side sync, falling back to raw-log-only reception: %v", m.cfg.Name, err)
				continue
			}
			if alreadyInstalled {
				logf("bridge %q: public meshcore channel (channel_hash %#x) already installed on the device at slot %d", m.cfg.Name, m.channelHash, meshcore.PublicChannelSlot)
			} else {
				logf("bridge %q: public meshcore channel (channel_hash %#x) installed on the device at slot %d", m.cfg.Name, m.channelHash, meshcore.PublicChannelSlot)
			}
			b.hashBySlot[meshcore.PublicChannelSlot] = m.channelHash
			continue
		}

		name := m.cfg.MeshCore.Hashtag
		if name == "" {
			name = m.cfg.Name
		}
		slot, alreadyInstalled, err := session.RegisterChannel(m.secret, name)
		if err != nil {
			logf("bridge %q: could not register meshcore channel %q (channel_hash %#x) for device-side sync, falling back to raw-log-only reception: %v", m.cfg.Name, name, m.channelHash, err)
			continue
		}
		if alreadyInstalled {
			logf("bridge %q: meshcore channel %q (channel_hash %#x) already installed on the device at slot %d", m.cfg.Name, name, m.channelHash, slot)
		} else {
			logf("bridge %q: meshcore channel %q (channel_hash %#x) installed on the device at slot %d", m.cfg.Name, name, m.channelHash, slot)
		}
		b.hashBySlot[slot] = m.channelHash
	}
}

// RunMeshtastic attaches session and consumes its decoded text messages
// until ctx is cancelled or the session closes, dispatching Meshtastic
// channel messages to Discord. The caller reconnects and calls this again
// on return; it does not affect any attached MeshCore session.
func (b *Bridge) RunMeshtastic(ctx context.Context, session *meshtastic.Session) error {
	b.SetMeshtasticSession(session)
	defer b.SetMeshtasticSession(nil)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.Done():
			return session.Err()
		case msg, ok := <-session.TextMessages():
			if !ok {
				return session.Err()
			}
			b.handleMeshtasticMessage(session, msg)
		}
	}
}

// RunHousekeeping periodically bounds the dedup/self-echo tracking maps.
// Runs for the lifetime of the process regardless of backend connectivity;
// call it once from cmd/hoplink.
func (b *Bridge) RunHousekeeping(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.sweep()
		}
	}
}

func (b *Bridge) sweep() {
	now := time.Now()
	ttl := b.effectiveSelfEchoTTL()
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, seen := range b.recentInbound {
		if now.Sub(seen) > inboundDedupTTL {
			delete(b.recentInbound, k)
		}
	}
	for k, pending := range b.recentOutbound {
		if now.Sub(pending.sentAt) > ttl {
			delete(b.recentOutbound, k)
		}
	}
}

// effectiveSelfEchoTTL returns how long a pending outbound-echo key survives
// before the periodic sweep purges it: selfEchoTTL, or either configured
// retry_wait_ms if longer — long enough that a slow-to-arrive repeat is
// never swept out from under retry_on_no_repeat's pending check (see
// echoUnheard), no matter how long retry_wait_ms is configured.
func (b *Bridge) effectiveSelfEchoTTL() time.Duration {
	ttl := selfEchoTTL
	if b.meshcoreRetryWait > ttl {
		ttl = b.meshcoreRetryWait
	}
	if b.meshtasticRetryWait > ttl {
		ttl = b.meshtasticRetryWait
	}
	return ttl
}

func logf(format string, args ...any) {
	log.Printf("[bridge] "+format, args...)
}

// debugf logs like logf but only when config's top-level debug: true is
// set — for extra-verbose per-message diagnostics (why an inbound message
// was suppressed) that would otherwise be noise in normal operation, since
// e.g. self-echo suppression fires on every message this bridge itself
// relays.
func (b *Bridge) debugf(format string, args ...any) {
	if b.debug {
		logf(format, args...)
	}
}
