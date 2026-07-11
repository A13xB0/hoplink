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

// selfEchoTTL bounds how long a just-sent outbound-echo key is remembered
// to suppress our own outbound message being re-delivered to Discord after
// the mesh floods it back to us.
const selfEchoTTL = 10 * time.Second

// mapping is one bridge entry: whichever of MeshCore/Meshtastic/Discord it
// has enabled (at least two of the three).
type mapping struct {
	cfg          config.Bridge
	webhook      *discord.WebhookSender // nil iff !discordEnabled
	maxBytes     int                    // resolved from cfg.MaxMessageBytes or the global default
	senderFormat string                 // resolved from cfg.SenderFormat or the global default; "none"/"short"/"full"

	discordEnabled bool

	meshcoreEnabled bool
	secret          []byte // MeshCore channel secret; valid iff meshcoreEnabled
	channelHash     byte   // valid iff meshcoreEnabled

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
	notify   notifier
	route    meshcore.RfRouteType
	hashSize int    // path hash bytes/hop for our outgoing MeshCore packets (1-3)
	scopeKey []byte // MeshCore flood scope key, or nil for unscoped ROUTE_TYPE_FLOOD
	byName   []*mapping
	byChan   map[string]*mapping // Discord channel ID -> mapping

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
	recentOutbound map[string]time.Time
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
	route, err := cfg.Meshcore.RouteType()
	if err != nil {
		return nil, err
	}

	b := &Bridge{
		route:          route,
		hashSize:       cfg.Meshcore.PathHashBytes,
		scopeKey:       cfg.Meshcore.ScopeKey(),
		txGuardEnabled: cfg.Coexistence.Enabled(),
		txGuardGap:     cfg.Coexistence.GapDuration(),
		byChan:         make(map[string]*mapping),
		recentInbound:  make(map[string]time.Time),
		recentOutbound: make(map[string]time.Time),
	}

	for _, bc := range cfg.Bridges {
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
			m.webhook = discord.NewWebhookSender(bc.DiscordWebhookURL)
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
func (b *Bridge) RunMeshcore(ctx context.Context, session *meshcore.Session) error {
	b.SetMeshcoreSession(session)
	defer b.SetMeshcoreSession(nil)

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
		}
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
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, seen := range b.recentInbound {
		if now.Sub(seen) > inboundDedupTTL {
			delete(b.recentInbound, k)
		}
	}
	for k, sent := range b.recentOutbound {
		if now.Sub(sent) > selfEchoTTL {
			delete(b.recentOutbound, k)
		}
	}
}

func logf(format string, args ...any) {
	log.Printf("[bridge] "+format, args...)
}
