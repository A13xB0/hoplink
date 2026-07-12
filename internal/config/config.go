// Package config loads and validates the hoplink YAML configuration: the
// MeshCore companion TCP endpoint, the optional Meshtastic device TCP
// endpoint, the optional Discord bot token, global message-size limits, and
// the list of bridges — each relaying between a MeshCore channel, a
// Meshtastic channel, and/or a Discord channel. A bridge needs at least two
// of the three; Discord is entirely optional, letting a bridge relay purely
// between MeshCore and Meshtastic with no Discord side at all.
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hectospark/hoplink/internal/meshcore"
)

// Config is the top-level hoplink configuration.
type Config struct {
	Meshcore    Meshcore    `yaml:"meshcore"`
	Meshtastic  Meshtastic  `yaml:"meshtastic"`
	Discord     Discord     `yaml:"discord"`
	Limits      Limits      `yaml:"limits"`
	Coexistence Coexistence `yaml:"coexistence"`
	// SenderFormat controls how a relayed message's origin surface
	// (Discord/MeshCore/Meshtastic) is indicated in the sender name shown
	// on each *other* destination: "none" (no tag, e.g. "Alice"), "short"
	// (e.g. "Alice (MC)"), or "full" (e.g. "Alice (MeshCore)"). Default
	// "none" — this must stay backward compatible with existing bridges.
	// Individual bridges may override this via Bridge.SenderFormat.
	SenderFormat string `yaml:"sender_format"`
	// Debug enables extra-verbose logging of every inbound message the
	// bridge suppresses and why (self-echo, duplicate delivery, or no
	// configured channel matched it) — silent by default since self-echo
	// suppression alone fires on every message this bridge itself relays.
	// Meant for diagnosing "a message reached other devices but never made
	// it to Discord/the other mesh". Default false.
	Debug   bool     `yaml:"debug"`
	Bridges []Bridge `yaml:"bridges"`
}

// validSenderFormat reports an error if s isn't a recognised sender_format
// value ("" counts as valid — it means "use the fallback/global value").
func validSenderFormat(s string) error {
	switch s {
	case "", "none", "short", "full":
		return nil
	default:
		return fmt.Errorf("sender_format must be \"none\", \"short\", or \"full\", got %q", s)
	}
}

// Meshcore holds the companion radio's TCP connection details. Required
// only if at least one bridge has meshcore.enabled: true.
type Meshcore struct {
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	AppName       string `yaml:"app_name"`
	PathHashBytes int    `yaml:"path_hash_bytes"` // 2 or 3 — bytes/hop for path tracking on our outgoing packets; 1-byte hashes are not allowed
	FloodScope    string `yaml:"flood_scope"`     // optional named flood scope/region; empty = unscoped ROUTE_TYPE_FLOOD
	// RxScopes optionally restricts which flood-scoped raw-log packets are
	// accepted for processing: a packet must carry a transport code matching
	// one of these scope names, or it's dropped. Empty (the default) accepts
	// every scope, scoped or not — today's behavior. Only applies to the
	// raw-log inbound path; the sync path's PACKET_CHANNEL_MSG_RECV carries
	// no route/transport-code metadata to filter on. Global default;
	// override per-bridge via bridges[].meshcore.rx_scopes.
	RxScopes []string `yaml:"rx_scopes"`
	// RetryOnNoRepeat, when true, has hoplink retransmit an outgoing MeshCore
	// message once if it never hears its own message repeated back by the
	// mesh (no self-echo observed) within a short window after sending — a
	// signal the original transmission likely wasn't picked up by any
	// repeater. Disabled by default: it's a heuristic (no repeater in range
	// is also normal on a small/direct-route mesh) and doubles airtime for
	// every message it triggers on.
	RetryOnNoRepeat bool `yaml:"retry_on_no_repeat"`
}

// Addr returns "host:port" for net.Dial.
func (m Meshcore) Addr() string {
	return fmt.Sprintf("%s:%d", m.Host, m.Port)
}

// ScopeKey resolves the 16-byte flood-scope key for FloodScope, or nil when
// unset (meaning unscoped ROUTE_TYPE_FLOOD, MeshCore's legacy default). This
// is the global default; a bridge may override it via
// Bridge.MeshCore.FloodScope (see Bridge.ResolvedScopeKey).
func (m Meshcore) ScopeKey() []byte {
	return scopeKeyForName(m.FloodScope)
}

// scopeKeyForName resolves a flood-scope name to its 16-byte key, or nil for
// a blank name (unscoped ROUTE_TYPE_FLOOD).
func scopeKeyForName(scope string) []byte {
	if strings.TrimSpace(scope) == "" {
		return nil
	}
	return meshcore.FloodScopeKey(scope)
}

// Meshtastic holds the attached Meshtastic device's TCP client-API
// connection details. Entirely optional — omit if you have no Meshtastic
// device; required only if at least one bridge has meshtastic.enabled: true.
type Meshtastic struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// HopLimit sets MeshPacket.hop_limit on every outgoing message — how many
	// times other nodes may rebroadcast it. Valid range 0-7 (a 3-bit field on
	// the wire; 7 is Meshtastic's own maximum). A *int (not int) so an unset
	// value can default to 7 while an explicit 0 (send to direct neighbours
	// only, never rebroadcast) is still honored rather than silently becoming
	// the default too — see ResolvedHopLimit.
	HopLimit *int `yaml:"hop_limit"`
}

// Addr returns "host:port" for net.Dial.
func (m Meshtastic) Addr() string {
	return fmt.Sprintf("%s:%d", m.Host, m.Port)
}

// Configured reports whether a Meshtastic connection has been given at all.
func (m Meshtastic) Configured() bool {
	return strings.TrimSpace(m.Host) != ""
}

// ResolvedHopLimit returns the effective hop_limit: HopLimit if explicitly
// set (including 0), else Meshtastic's own maximum (7).
func (m Meshtastic) ResolvedHopLimit() int {
	if m.HopLimit == nil {
		return 7
	}
	return *m.HopLimit
}

// Discord holds the bot's gateway credentials. The whole block is optional
// — omit it entirely (and leave every bridge's discord_channel_id/
// discord_webhook_url unset) to run hoplink as a pure MeshCore<->Meshtastic
// bridge with no Discord side at all.
type Discord struct {
	BotToken   string `yaml:"bot_token"`
	NameSource string `yaml:"name_source"` // "display_name" (default) or "username"
}

// PreferDisplayName reports whether NameSource resolves to using the
// account's display name (nick > global display name > username) rather
// than the raw username.
func (d Discord) PreferDisplayName() (bool, error) {
	switch d.NameSource {
	case "display_name", "":
		return true, nil
	case "username":
		return false, nil
	default:
		return false, fmt.Errorf("config: discord.name_source must be \"display_name\" or \"username\", got %q", d.NameSource)
	}
}

// Limits holds global outbound message-size limits.
type Limits struct {
	// MaxMessageBytes caps the composed "<Name>: <content>" text (before
	// chunking) that a bridge will relay from Discord to the mesh. Oversized
	// messages are rejected outright (not chunked) and the sender is told
	// why, in Discord only. Individual bridges may override this via
	// Bridge.MaxMessageBytes.
	MaxMessageBytes int `yaml:"max_message_bytes"`
}

// Coexistence controls best-effort mitigation of RF interference between a
// co-located MeshCore radio and Meshtastic device transmitting at the same
// time. This can only reduce the odds of overlap, not guarantee it: neither
// protocol reports back exact transmit-start/transmit-complete timing, so
// hoplink can only avoid *issuing* both sends at the same instant and pad
// a settle gap afterward — actual on-air airtime can still exceed that gap.
type Coexistence struct {
	// AvoidSimultaneousTX serialises all outbound MeshCore and Meshtastic
	// sends (across every bridge) through a single lock, so this process
	// never asks both radios to transmit at once. Default true.
	AvoidSimultaneousTX *bool `yaml:"avoid_simultaneous_tx"`
	// MinGapMs is an extra pause (milliseconds) held after each send before
	// the next one may start, approximating airtime settle time. Default 100.
	MinGapMs int `yaml:"min_gap_ms"`
}

// Enabled reports whether simultaneous-TX avoidance is on (defaults true
// when unset, since AvoidSimultaneousTX is a *bool to distinguish "not set"
// from an explicit false).
func (c Coexistence) Enabled() bool {
	return c.AvoidSimultaneousTX == nil || *c.AvoidSimultaneousTX
}

// GapDuration returns MinGapMs as a time.Duration.
func (c Coexistence) GapDuration() time.Duration {
	return time.Duration(c.MinGapMs) * time.Millisecond
}

// BridgeMeshCore is a bridge's MeshCore-side configuration: which channel
// (hashtag, explicit secret, or the well-known public channel) to relay.
type BridgeMeshCore struct {
	Enabled   bool   `yaml:"enabled"`
	Hashtag   string `yaml:"hashtag"`
	SecretHex string `yaml:"secret_hex"`
	Public    bool   `yaml:"public"`
	// FloodScope optionally overrides the top-level meshcore.flood_scope for
	// this bridge only; "" means use the global default (see
	// Bridge.ResolvedScopeKey).
	FloodScope string `yaml:"flood_scope"`
	// ReadOnly, when true, means this bridge only ever receives from
	// MeshCore — messages from Discord or a linked Meshtastic channel are
	// never transmitted out over this bridge's MeshCore side.
	ReadOnly bool `yaml:"read_only"`
	// RxScopes optionally overrides the top-level meshcore.rx_scopes for
	// this bridge only; empty means use the global default (see
	// Bridge.ResolvedRxScopes).
	RxScopes []string `yaml:"rx_scopes"`
}

// BridgeMeshtastic is a bridge's Meshtastic-side configuration: which
// channel to relay, by name. That channel must already exist as a slot on
// the attached Meshtastic device (see internal/meshtastic's package docs)
// — this bridge cannot create one.
type BridgeMeshtastic struct {
	Enabled     bool   `yaml:"enabled"`
	ChannelName string `yaml:"channel_name"`
	// ReadOnly, when true, means this bridge only ever receives from
	// Meshtastic — messages from Discord or a linked MeshCore channel are
	// never transmitted out over this bridge's Meshtastic side.
	ReadOnly bool `yaml:"read_only"`
}

// Bridge relays between a MeshCore channel, a Meshtastic channel, and/or a
// Discord text channel + webhook — each side independently toggled
// (MeshCore.Enabled / Meshtastic.Enabled / a non-empty DiscordChannelID). A
// bridge needs at least two of the three sides: Discord+MeshCore,
// Discord+Meshtastic, MeshCore+Meshtastic (Discord omitted entirely), or all
// three. Any side can be made ReadOnly (receive-only, never transmits). Two
// or more bridges that reference the same MeshCore channel (matching
// secret) and/or the same Meshtastic channel_name are treated as siblings:
// a message landing on any one of them relays directly to every sibling's
// Discord channel too — e.g. bridging one MeshCore channel into two
// different Discord guilds.
type Bridge struct {
	Name string `yaml:"name"`
	// Enabled toggles this whole bridge entry on or off; nil/unset defaults
	// to true (see IsEnabled). Set to false to keep a not-yet-finished or
	// temporarily-unwanted bridge in the config without it being built,
	// connected to, or validated for completeness — its meshcore/meshtastic/
	// discord fields may be partial or missing while disabled.
	Enabled *bool `yaml:"enabled"`
	// DiscordChannelID and DiscordWebhookURL are optional: leave both empty
	// for a bridge with no Discord side (a pure MeshCore<->Meshtastic relay).
	// If either is set, both must be.
	DiscordChannelID  string `yaml:"discord_channel_id"`
	DiscordWebhookURL string `yaml:"discord_webhook_url"`
	GuildID           string `yaml:"guild_id"`          // optional; if set, incoming messages from a different guild are ignored (defensive check, not required for routing correctness — Discord channel IDs are already globally unique)
	MaxMessageBytes   int    `yaml:"max_message_bytes"` // optional per-bridge override of limits.max_message_bytes; 0 = use the global default
	SenderFormat      string `yaml:"sender_format"`     // optional per-bridge override of the top-level sender_format; "" = use the global value
	// DiscordReadOnly, when true, means this bridge's Discord channel only
	// ever receives posts — messages typed there are never relayed out to
	// the mesh, or to a sibling bridge's Discord channel sharing the same
	// MeshCore/Meshtastic channel.
	DiscordReadOnly bool `yaml:"discord_read_only"`

	MeshCore   BridgeMeshCore   `yaml:"meshcore"`
	Meshtastic BridgeMeshtastic `yaml:"meshtastic"`
}

// IsEnabled reports whether this bridge is active (defaults to true when
// Enabled is unset).
func (b Bridge) IsEnabled() bool {
	return b.Enabled == nil || *b.Enabled
}

// Secret resolves this bridge's 16-byte MeshCore channel secret from
// whichever of MeshCore.Hashtag / MeshCore.SecretHex / MeshCore.Public was
// configured. Only meaningful when MeshCore.Enabled.
func (b Bridge) Secret() ([]byte, error) {
	switch {
	case b.MeshCore.Hashtag != "":
		return meshcore.HashtagChannelSecret(b.MeshCore.Hashtag), nil
	case b.MeshCore.SecretHex != "":
		secret, err := hex.DecodeString(b.MeshCore.SecretHex)
		if err != nil {
			return nil, fmt.Errorf("config: bridge %q: meshcore.secret_hex is not valid hex: %w", b.Name, err)
		}
		if len(secret) != 16 {
			return nil, fmt.Errorf("config: bridge %q: meshcore.secret_hex must decode to 16 bytes, got %d", b.Name, len(secret))
		}
		return secret, nil
	case b.MeshCore.Public:
		return meshcore.PublicChannelKey, nil
	default:
		return nil, fmt.Errorf("config: bridge %q: exactly one of meshcore.hashtag, meshcore.secret_hex, or meshcore.public must be set", b.Name)
	}
}

// ResolvedMaxMessageBytes returns this bridge's effective byte limit: its
// own MaxMessageBytes override if set (>0), else global.
func (b Bridge) ResolvedMaxMessageBytes(global int) int {
	if b.MaxMessageBytes > 0 {
		return b.MaxMessageBytes
	}
	return global
}

// ResolvedSenderFormat returns this bridge's effective sender_format: its
// own override if set, else global.
func (b Bridge) ResolvedSenderFormat(global string) string {
	if b.SenderFormat != "" {
		return b.SenderFormat
	}
	return global
}

// ResolvedScopeKey returns this bridge's effective MeshCore flood-scope key:
// derived from its own meshcore.flood_scope override if set, else from
// globalScope (the top-level meshcore.flood_scope). Only meaningful when
// MeshCore.Enabled.
func (b Bridge) ResolvedScopeKey(globalScope string) []byte {
	scope := b.MeshCore.FloodScope
	if scope == "" {
		scope = globalScope
	}
	return scopeKeyForName(scope)
}

// ResolvedRxScopes returns this bridge's effective rx_scopes allowlist: its
// own meshcore.rx_scopes override if non-empty, else globalScopes (the
// top-level meshcore.rx_scopes). An empty result means accept every scope
// (no filtering) — the default.
func (b Bridge) ResolvedRxScopes(globalScopes []string) []string {
	if len(b.MeshCore.RxScopes) > 0 {
		return b.MeshCore.RxScopes
	}
	return globalScopes
}

// DiscordEnabled reports whether any enabled bridge has a Discord side
// configured. When false, hoplink runs with no Discord gateway connection at
// all — purely relaying between MeshCore and Meshtastic.
func (c *Config) DiscordEnabled() bool {
	for _, b := range c.Bridges {
		if b.IsEnabled() && strings.TrimSpace(b.DiscordChannelID) != "" {
			return true
		}
	}
	return false
}

// Load reads and validates a hoplink config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Meshcore.Port == 0 {
		c.Meshcore.Port = 5000
	}
	if c.Meshcore.AppName == "" {
		c.Meshcore.AppName = "hoplink"
	}
	if c.Meshcore.PathHashBytes == 0 {
		c.Meshcore.PathHashBytes = 3
	}
	if c.Meshtastic.Port == 0 {
		c.Meshtastic.Port = 4403
	}
	if c.Limits.MaxMessageBytes == 0 {
		c.Limits.MaxMessageBytes = 320
	}
	if c.Coexistence.MinGapMs == 0 {
		c.Coexistence.MinGapMs = 100
	}
	if c.SenderFormat == "" {
		c.SenderFormat = "none"
	}
}

// Validate checks the config for structural and semantic errors, returning
// every problem found (not just the first) so users can fix them all at once.
func (c *Config) Validate() error {
	var errs []string

	anyMeshcore, anyMeshtastic, anyDiscord := false, false, false
	for _, b := range c.Bridges {
		if !b.IsEnabled() {
			continue // a disabled bridge's backend toggles don't require anything to be configured
		}
		anyMeshcore = anyMeshcore || b.MeshCore.Enabled
		anyMeshtastic = anyMeshtastic || b.Meshtastic.Enabled
		anyDiscord = anyDiscord || strings.TrimSpace(b.DiscordChannelID) != ""
	}

	if anyMeshcore {
		if strings.TrimSpace(c.Meshcore.Host) == "" {
			errs = append(errs, "meshcore.host is required because at least one bridge has meshcore.enabled: true")
		}
		// 1-byte path hashes are deliberately excluded: they're the protocol's
		// legacy default but collide far more often on larger meshes, so this
		// bridge always relays with 2- or 3-byte hop hashes.
		if c.Meshcore.PathHashBytes < 2 || c.Meshcore.PathHashBytes > 3 {
			errs = append(errs, fmt.Sprintf("meshcore.path_hash_bytes must be 2 or 3 (1-byte path hashes are not allowed), got %d", c.Meshcore.PathHashBytes))
		}
	}
	if anyMeshtastic {
		if !c.Meshtastic.Configured() {
			errs = append(errs, "meshtastic.host is required because at least one bridge has meshtastic.enabled: true")
		}
		if hl := c.Meshtastic.ResolvedHopLimit(); hl < 0 || hl > 7 {
			errs = append(errs, fmt.Sprintf("meshtastic.hop_limit must be between 0 and 7, got %d", hl))
		}
	}

	if anyDiscord && strings.TrimSpace(c.Discord.BotToken) == "" {
		errs = append(errs, "discord.bot_token is required because at least one bridge has discord_channel_id set")
	}
	if _, err := c.Discord.PreferDisplayName(); err != nil {
		errs = append(errs, err.Error())
	}
	if c.Limits.MaxMessageBytes <= 0 {
		errs = append(errs, fmt.Sprintf("limits.max_message_bytes must be positive, got %d", c.Limits.MaxMessageBytes))
	}
	if c.Coexistence.MinGapMs < 0 {
		errs = append(errs, fmt.Sprintf("coexistence.min_gap_ms must not be negative, got %d", c.Coexistence.MinGapMs))
	}
	if err := validSenderFormat(c.SenderFormat); err != nil {
		errs = append(errs, "sender_format: "+err.Error())
	}
	if len(c.Bridges) == 0 {
		errs = append(errs, "at least one entry in bridges is required")
	}

	seenNames := map[string]bool{}
	seenChannelIDs := map[string]bool{}
	for i, b := range c.Bridges {
		label := b.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}
		if b.Name == "" {
			errs = append(errs, fmt.Sprintf("bridges[%s]: name is required", label))
		} else if seenNames[b.Name] {
			errs = append(errs, fmt.Sprintf("bridges[%s]: duplicate bridge name", label))
		}
		seenNames[b.Name] = true

		if !b.IsEnabled() {
			// A disabled bridge may be an incomplete work-in-progress; skip
			// every other check below so it doesn't block loading the rest
			// of the config. It's also excluded from bridge.New entirely.
			continue
		}

		if !b.MeshCore.Enabled && !b.Meshtastic.Enabled {
			errs = append(errs, fmt.Sprintf("bridges[%s]: at least one of meshcore.enabled or meshtastic.enabled must be true", label))
		}

		if b.MeshCore.Enabled {
			set := 0
			if b.MeshCore.Hashtag != "" {
				set++
			}
			if b.MeshCore.SecretHex != "" {
				set++
			}
			if b.MeshCore.Public {
				set++
			}
			if set != 1 {
				errs = append(errs, fmt.Sprintf("bridges[%s].meshcore: exactly one of hashtag, secret_hex, or public must be set (got %d)", label, set))
			} else if _, err := b.Secret(); err != nil {
				errs = append(errs, err.Error())
			}
		}

		if b.Meshtastic.Enabled && strings.TrimSpace(b.Meshtastic.ChannelName) == "" {
			errs = append(errs, fmt.Sprintf("bridges[%s].meshtastic: channel_name is required", label))
		}

		if b.MaxMessageBytes < 0 {
			errs = append(errs, fmt.Sprintf("bridges[%s]: max_message_bytes must not be negative", label))
		}

		if err := validSenderFormat(b.SenderFormat); err != nil {
			errs = append(errs, fmt.Sprintf("bridges[%s].sender_format: %s", label, err.Error()))
		}

		hasChannel := strings.TrimSpace(b.DiscordChannelID) != ""
		hasWebhook := strings.TrimSpace(b.DiscordWebhookURL) != ""
		if hasChannel != hasWebhook {
			errs = append(errs, fmt.Sprintf("bridges[%s]: discord_channel_id and discord_webhook_url must both be set, or both left empty (empty means this bridge has no Discord side)", label))
		}
		// Two bridges sharing a discord_channel_id can't both work: inbound
		// Discord routing is keyed by channel ID (see bridge.New's byChan), so
		// the second silently wins and the first's Discord->mesh direction goes
		// dead. Siblings are meant to share a *mesh* channel across *different*
		// Discord channels, never the same Discord channel.
		if hasChannel {
			id := strings.TrimSpace(b.DiscordChannelID)
			if seenChannelIDs[id] {
				errs = append(errs, fmt.Sprintf("bridges[%s]: duplicate discord_channel_id %q (each Discord channel may be bridged by at most one bridge)", label, id))
			}
			seenChannelIDs[id] = true
		}
		if !hasChannel && !(b.MeshCore.Enabled && b.Meshtastic.Enabled) {
			errs = append(errs, fmt.Sprintf("bridges[%s]: a bridge with no Discord side must have both meshcore.enabled and meshtastic.enabled set (otherwise it has nothing to relay between)", label))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("config: invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}
