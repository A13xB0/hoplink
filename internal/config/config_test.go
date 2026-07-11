package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hectospark/hoplink/internal/meshcore"
)

func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

const validMinimal = `
meshcore:
  host: 192.168.4.1
discord:
  bot_token: "abc123"
bridges:
  - name: general
    discord_channel_id: "123"
    discord_webhook_url: "https://discord.com/api/webhooks/x/y"
    meshcore:
      enabled: true
      hashtag: "#general"
`

// configWithMeshcore builds a full, valid config document with a single
// meshcore block (host plus any extraFields), avoiding YAML's rejection of a
// duplicate top-level "meshcore" key that a naive validMinimal+"..." append
// would produce.
func configWithMeshcore(extraFields string) string {
	return fmt.Sprintf(`
meshcore:
  host: 192.168.4.1
%s
discord:
  bot_token: "abc123"
bridges:
  - name: general
    discord_channel_id: "123"
    discord_webhook_url: "https://discord.com/api/webhooks/x/y"
    meshcore:
      enabled: true
      hashtag: "#general"
`, extraFields)
}

// configWithDiscord is configWithMeshcore's counterpart for the discord
// block, avoiding the same duplicate-key problem.
func configWithDiscord(extraFields string) string {
	return fmt.Sprintf(`
meshcore:
  host: 192.168.4.1
discord:
  bot_token: "abc123"
%s
bridges:
  - name: general
    discord_channel_id: "123"
    discord_webhook_url: "https://discord.com/api/webhooks/x/y"
    meshcore:
      enabled: true
      hashtag: "#general"
`, extraFields)
}

// configWithSenderFormat sets the top-level scalar sender_format field.
func configWithSenderFormat(value string) string {
	return fmt.Sprintf(`
meshcore:
  host: 192.168.4.1
discord:
  bot_token: "abc123"
sender_format: %s
bridges:
  - name: general
    discord_channel_id: "123"
    discord_webhook_url: "https://discord.com/api/webhooks/x/y"
    meshcore:
      enabled: true
      hashtag: "#general"
`, value)
}

// configWithLimits is configWithMeshcore's counterpart for the top-level
// limits block.
func configWithLimits(extraFields string) string {
	return fmt.Sprintf(`
meshcore:
  host: 192.168.4.1
discord:
  bot_token: "abc123"
limits:
%s
bridges:
  - name: general
    discord_channel_id: "123"
    discord_webhook_url: "https://discord.com/api/webhooks/x/y"
    meshcore:
      enabled: true
      hashtag: "#general"
`, extraFields)
}

func TestLoad_AppliesDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, validMinimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Meshcore.Port != 5000 {
		t.Errorf("Port = %d, want 5000", cfg.Meshcore.Port)
	}
	if cfg.Meshcore.AppName != "hoplink" {
		t.Errorf("AppName = %q, want hoplink", cfg.Meshcore.AppName)
	}
	if cfg.Meshcore.Route != "flood" {
		t.Errorf("Route = %q, want flood", cfg.Meshcore.Route)
	}
	if cfg.Meshcore.Addr() != "192.168.4.1:5000" {
		t.Errorf("Addr = %q, want 192.168.4.1:5000", cfg.Meshcore.Addr())
	}
	if cfg.Meshtastic.Port != 4403 {
		t.Errorf("Meshtastic.Port = %d, want 4403", cfg.Meshtastic.Port)
	}
	if cfg.Limits.MaxMessageBytes != 320 {
		t.Errorf("Limits.MaxMessageBytes = %d, want 320", cfg.Limits.MaxMessageBytes)
	}
}

func TestLoad_DefaultsPathHashBytesToThree(t *testing.T) {
	cfg, err := Load(writeTemp(t, validMinimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Meshcore.PathHashBytes != 3 {
		t.Errorf("PathHashBytes = %d, want 3 (never default to 1-byte path hashes)", cfg.Meshcore.PathHashBytes)
	}
}

func TestLoad_RejectsOneBytePathHash(t *testing.T) {
	cfg := configWithMeshcore("  path_hash_bytes: 1")
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "path_hash_bytes") {
		t.Fatalf("expected path_hash_bytes error, got %v", err)
	}
}

func TestLoad_RejectsOutOfRangePathHashBytes(t *testing.T) {
	cfg := configWithMeshcore("  path_hash_bytes: 4")
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "path_hash_bytes") {
		t.Fatalf("expected path_hash_bytes error, got %v", err)
	}
}

func TestLoad_AcceptsExplicitTwoOrThreeBytePathHash(t *testing.T) {
	for _, n := range []int{2, 3} {
		cfg := configWithMeshcore(fmt.Sprintf("  path_hash_bytes: %d", n))
		got, err := Load(writeTemp(t, cfg))
		if err != nil {
			t.Fatalf("Load(path_hash_bytes=%d): %v", n, err)
		}
		if got.Meshcore.PathHashBytes != n {
			t.Errorf("PathHashBytes = %d, want %d", got.Meshcore.PathHashBytes, n)
		}
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/path/config.yaml"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_RejectsBadRoute(t *testing.T) {
	cfg := configWithMeshcore("  route: sideways")
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "route") {
		t.Fatalf("expected a route error, got %v", err)
	}
}

func TestLoad_RequiresMeshcoreHostOnlyWhenMeshcoreEnabled(t *testing.T) {
	cfg := `
discord:
  bot_token: "abc"
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshcore:
      enabled: true
      hashtag: "#general"
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "meshcore.host") {
		t.Fatalf("expected meshcore.host error, got %v", err)
	}
}

func TestLoad_RequiresBotToken(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshcore:
      enabled: true
      hashtag: "#general"
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "bot_token") {
		t.Fatalf("expected bot_token error, got %v", err)
	}
}

func TestLoad_AllowsBridgeWithNoDiscordSideWhenBothMeshBackendsEnabled(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
meshtastic:
  host: 5.6.7.8
bridges:
  - name: general
    meshcore:
      enabled: true
      hashtag: "#general"
    meshtastic:
      enabled: true
      channel_name: "LongFast"
`
	got, err := Load(writeTemp(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DiscordEnabled() {
		t.Error("DiscordEnabled() should be false when no bridge sets discord_channel_id")
	}
}

func TestLoad_RejectsNoDiscordBridgeWithOnlyOneMeshBackendEnabled(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
bridges:
  - name: general
    meshcore:
      enabled: true
      hashtag: "#general"
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "must have both meshcore.enabled and meshtastic.enabled set") {
		t.Fatalf("expected a no-discord-needs-both-backends error, got %v", err)
	}
}

func TestLoad_RejectsMismatchedDiscordChannelAndWebhook(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
meshtastic:
  host: 5.6.7.8
bridges:
  - name: general
    discord_channel_id: "123"
    meshcore:
      enabled: true
      hashtag: "#general"
    meshtastic:
      enabled: true
      channel_name: "LongFast"
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "must both be set, or both left empty") {
		t.Fatalf("expected a mismatched-discord-fields error, got %v", err)
	}
}

func TestLoad_DoesNotRequireBotTokenWhenNoBridgeUsesDiscord(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
meshtastic:
  host: 5.6.7.8
bridges:
  - name: general
    meshcore:
      enabled: true
      hashtag: "#general"
    meshtastic:
      enabled: true
      channel_name: "LongFast"
`
	if _, err := Load(writeTemp(t, cfg)); err != nil {
		t.Fatalf("Load should succeed with no discord.bot_token when no bridge uses Discord: %v", err)
	}
}

func TestDiscord_PreferDisplayName_DefaultsTrue(t *testing.T) {
	got, err := Discord{}.PreferDisplayName()
	if err != nil {
		t.Fatalf("PreferDisplayName: %v", err)
	}
	if !got {
		t.Error("PreferDisplayName() should default to true when name_source is unset")
	}
}

func TestDiscord_PreferDisplayName_ExplicitValues(t *testing.T) {
	cases := []struct {
		nameSource string
		want       bool
	}{
		{"display_name", true},
		{"username", false},
	}
	for _, c := range cases {
		got, err := Discord{NameSource: c.nameSource}.PreferDisplayName()
		if err != nil {
			t.Fatalf("PreferDisplayName(%q): %v", c.nameSource, err)
		}
		if got != c.want {
			t.Errorf("PreferDisplayName(%q) = %v, want %v", c.nameSource, got, c.want)
		}
	}
}

func TestDiscord_PreferDisplayName_RejectsUnknownValue(t *testing.T) {
	if _, err := (Discord{NameSource: "nickname"}).PreferDisplayName(); err == nil {
		t.Fatal("expected an error for an unrecognised name_source")
	}
}

func TestLoad_RejectsUnknownNameSource(t *testing.T) {
	cfg := configWithDiscord("  name_source: nickname")
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "name_source") {
		t.Fatalf("expected name_source error, got %v", err)
	}
}

func TestLoad_AcceptsExplicitNameSourceUsername(t *testing.T) {
	cfg := configWithDiscord("  name_source: username")
	got, err := Load(writeTemp(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prefer, err := got.Discord.PreferDisplayName()
	if err != nil {
		t.Fatalf("PreferDisplayName: %v", err)
	}
	if prefer {
		t.Error("expected name_source: username to resolve to PreferDisplayName() == false")
	}
}

func TestLoad_RequiresAtLeastOneBridge(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
discord:
  bot_token: abc
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "at least one entry in bridges") {
		t.Fatalf("expected bridges-required error, got %v", err)
	}
}

func TestBridge_IsEnabled_DefaultsTrueWhenUnset(t *testing.T) {
	if !(Bridge{}).IsEnabled() {
		t.Error("IsEnabled() should default to true when Enabled is unset")
	}
}

func TestBridge_IsEnabled_ExplicitFalse(t *testing.T) {
	falseVal := false
	if (Bridge{Enabled: &falseVal}).IsEnabled() {
		t.Error("IsEnabled() should be false when Enabled: false")
	}
}

func TestBridge_IsEnabled_ExplicitTrue(t *testing.T) {
	trueVal := true
	if !(Bridge{Enabled: &trueVal}).IsEnabled() {
		t.Error("IsEnabled() should be true when Enabled: true")
	}
}

func TestLoad_DisabledBridgeSkipsContentValidation(t *testing.T) {
	// An incomplete, not-yet-finished bridge (no secret source, no
	// discord_webhook_url, no backend enabled at all) must not block Load
	// as long as it's disabled.
	cfg := `
discord:
  bot_token: abc
bridges:
  - name: wip
    enabled: false
    discord_channel_id: "1"
    meshcore:
      enabled: true
`
	if _, err := Load(writeTemp(t, cfg)); err != nil {
		t.Fatalf("Load should succeed for a disabled, incomplete bridge: %v", err)
	}
}

func TestLoad_DisabledBridgeDoesNotRequireMeshcoreHost(t *testing.T) {
	cfg := `
discord:
  bot_token: abc
bridges:
  - name: wip
    enabled: false
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshcore:
      enabled: true
      hashtag: "#general"
`
	if _, err := Load(writeTemp(t, cfg)); err != nil {
		t.Fatalf("Load should not require meshcore.host for a disabled bridge: %v", err)
	}
}

func TestLoad_DisabledBridgeStillChecksNameUniqueness(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshcore:
      enabled: true
      hashtag: "#general"
  - name: general
    enabled: false
    meshcore:
      enabled: true
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "duplicate bridge name") {
		t.Fatalf("expected a duplicate-name error even though the second bridge is disabled, got %v", err)
	}
}

func TestLoad_RejectsBridgeWithNeitherBackendEnabled(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "at least one of meshcore.enabled or meshtastic.enabled") {
		t.Fatalf("expected an enabled-backend error, got %v", err)
	}
}

func TestLoad_RejectsBridgeWithNoSecretSource(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshcore:
      enabled: true
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "exactly one of hashtag, secret_hex, or public") {
		t.Fatalf("expected secret-source error, got %v", err)
	}
}

func TestLoad_RejectsBridgeWithMultipleSecretSources(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshcore:
      enabled: true
      hashtag: "#general"
      public: true
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "exactly one of hashtag, secret_hex, or public") {
		t.Fatalf("expected secret-source error, got %v", err)
	}
}

func TestLoad_RejectsDuplicateBridgeNames(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshcore:
      enabled: true
      hashtag: "#general"
  - name: general
    discord_channel_id: "2"
    discord_webhook_url: "https://y"
    meshcore:
      enabled: true
      hashtag: "#other"
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "duplicate bridge name") {
		t.Fatalf("expected duplicate-name error, got %v", err)
	}
}

func TestLoad_RejectsBadSecretHex(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshcore:
      enabled: true
      secret_hex: "not-hex"
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil {
		t.Fatal("expected error for invalid secret_hex")
	}
}

func TestLoad_RejectsWrongLengthSecretHex(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshcore:
      enabled: true
      secret_hex: "aabb"
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil {
		t.Fatal("expected error for wrong-length secret_hex")
	}
}

func TestBridge_Secret_Hashtag(t *testing.T) {
	b := Bridge{Name: "x", MeshCore: BridgeMeshCore{Enabled: true, Hashtag: "#test"}}
	secret, err := b.Secret()
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	want, _ := hex.DecodeString("9cd8fcf22a47333b591d96a2b848b73f")
	if hex.EncodeToString(secret) != hex.EncodeToString(want) {
		t.Errorf("secret = %x, want %x", secret, want)
	}
}

func TestBridge_Secret_Public(t *testing.T) {
	b := Bridge{Name: "x", MeshCore: BridgeMeshCore{Enabled: true, Public: true}}
	secret, err := b.Secret()
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	want, _ := hex.DecodeString("8b3387e9c5cdea6ac9e5edbaa115cd72")
	if hex.EncodeToString(secret) != hex.EncodeToString(want) {
		t.Errorf("secret = %x, want %x", secret, want)
	}
}

func TestBridge_Secret_Explicit(t *testing.T) {
	secretHex := "00112233445566778899aabbccddeeff"[:32]
	b := Bridge{Name: "x", MeshCore: BridgeMeshCore{Enabled: true, SecretHex: secretHex}}
	secret, err := b.Secret()
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	want, _ := hex.DecodeString(secretHex)
	if hex.EncodeToString(secret) != hex.EncodeToString(want) {
		t.Errorf("secret = %x, want %x", secret, want)
	}
}

func TestBridge_ResolvedMaxMessageBytes_UsesOverrideWhenSet(t *testing.T) {
	b := Bridge{MaxMessageBytes: 480}
	if got := b.ResolvedMaxMessageBytes(320); got != 480 {
		t.Errorf("ResolvedMaxMessageBytes = %d, want 480", got)
	}
}

func TestBridge_ResolvedMaxMessageBytes_FallsBackToGlobal(t *testing.T) {
	b := Bridge{}
	if got := b.ResolvedMaxMessageBytes(320); got != 320 {
		t.Errorf("ResolvedMaxMessageBytes = %d, want 320", got)
	}
}

func TestMeshcore_ScopeKey_UnsetIsNil(t *testing.T) {
	m := Meshcore{}
	if got := m.ScopeKey(); got != nil {
		t.Errorf("ScopeKey() = %x, want nil for an unset flood_scope", got)
	}
}

func TestMeshcore_ScopeKey_BlankIsNil(t *testing.T) {
	m := Meshcore{FloodScope: "   "}
	if got := m.ScopeKey(); got != nil {
		t.Errorf("ScopeKey() = %x, want nil for a blank flood_scope", got)
	}
}

func TestMeshcore_ScopeKey_ResolvesToFloodScopeKey(t *testing.T) {
	m := Meshcore{FloodScope: "myregion"}
	got := m.ScopeKey()
	want := meshcore.FloodScopeKey("myregion")
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Errorf("ScopeKey() = %x, want %x", got, want)
	}
}

func TestLoad_FloodScopeDefaultsToUnset(t *testing.T) {
	cfg, err := Load(writeTemp(t, validMinimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Meshcore.FloodScope != "" {
		t.Errorf("FloodScope = %q, want empty by default", cfg.Meshcore.FloodScope)
	}
	if cfg.Meshcore.ScopeKey() != nil {
		t.Error("ScopeKey() should be nil when flood_scope is unset")
	}
}

func TestLoad_FloodScopeIsConfigurable(t *testing.T) {
	cfg := configWithMeshcore(`  flood_scope: "myregion"`)
	got, err := Load(writeTemp(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Meshcore.FloodScope != "myregion" {
		t.Errorf("FloodScope = %q, want %q", got.Meshcore.FloodScope, "myregion")
	}
	want := meshcore.FloodScopeKey("myregion")
	if hex.EncodeToString(got.Meshcore.ScopeKey()) != hex.EncodeToString(want) {
		t.Errorf("ScopeKey() = %x, want %x", got.Meshcore.ScopeKey(), want)
	}
}

func TestBridge_ResolvedScopeKey_UsesGlobalWhenBridgeUnset(t *testing.T) {
	b := Bridge{MeshCore: BridgeMeshCore{Enabled: true}}
	got := b.ResolvedScopeKey("globalregion")
	want := meshcore.FloodScopeKey("globalregion")
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Errorf("ResolvedScopeKey() = %x, want global's %x", got, want)
	}
}

func TestBridge_ResolvedScopeKey_BridgeOverrideWinsOverGlobal(t *testing.T) {
	b := Bridge{MeshCore: BridgeMeshCore{Enabled: true, FloodScope: "bridgeregion"}}
	got := b.ResolvedScopeKey("globalregion")
	want := meshcore.FloodScopeKey("bridgeregion")
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Errorf("ResolvedScopeKey() = %x, want bridge override's %x", got, want)
	}
}

func TestBridge_ResolvedScopeKey_NilWhenBothUnset(t *testing.T) {
	b := Bridge{MeshCore: BridgeMeshCore{Enabled: true}}
	if got := b.ResolvedScopeKey(""); got != nil {
		t.Errorf("ResolvedScopeKey() = %x, want nil when neither bridge nor global flood_scope is set", got)
	}
}

func TestLoad_BridgeFloodScopeOverridesGlobal(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
  flood_scope: "globalregion"
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshcore:
      enabled: true
      hashtag: "#general"
      flood_scope: "bridgeregion"
`
	got, err := Load(writeTemp(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b := got.Bridges[0]
	if b.MeshCore.FloodScope != "bridgeregion" {
		t.Errorf("MeshCore.FloodScope = %q, want %q", b.MeshCore.FloodScope, "bridgeregion")
	}
	want := meshcore.FloodScopeKey("bridgeregion")
	if hex.EncodeToString(b.ResolvedScopeKey(got.Meshcore.FloodScope)) != hex.EncodeToString(want) {
		t.Errorf("ResolvedScopeKey() = %x, want the bridge override %x, not the global scope", b.ResolvedScopeKey(got.Meshcore.FloodScope), want)
	}
}

func TestMeshtastic_Configured(t *testing.T) {
	if (Meshtastic{}).Configured() {
		t.Error("Configured() should be false when host is unset")
	}
	if !(Meshtastic{Host: "10.0.0.5"}).Configured() {
		t.Error("Configured() should be true when host is set")
	}
}

func TestLoad_RequiresMeshtasticHostOnlyWhenMeshtasticEnabled(t *testing.T) {
	cfg := `
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshtastic:
      enabled: true
      channel_name: "general"
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "meshtastic.host") {
		t.Fatalf("expected meshtastic.host error, got %v", err)
	}
}

func TestLoad_AcceptsMeshtasticOnlyBridge(t *testing.T) {
	cfg := `
meshtastic:
  host: 10.0.0.5
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshtastic:
      enabled: true
      channel_name: "general"
`
	got, err := Load(writeTemp(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Meshtastic.Addr() != "10.0.0.5:4403" {
		t.Errorf("Meshtastic.Addr() = %q, want 10.0.0.5:4403", got.Meshtastic.Addr())
	}
	if !got.Bridges[0].Meshtastic.Enabled || got.Bridges[0].Meshtastic.ChannelName != "general" {
		t.Errorf("unexpected bridge meshtastic config: %+v", got.Bridges[0].Meshtastic)
	}
}

func TestLoad_RejectsMeshtasticEnabledWithoutChannelName(t *testing.T) {
	cfg := `
meshtastic:
  host: 10.0.0.5
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshtastic:
      enabled: true
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "channel_name") {
		t.Fatalf("expected channel_name error, got %v", err)
	}
}

func TestLoad_AcceptsBridgeWithBothBackendsEnabled(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
meshtastic:
  host: 10.0.0.5
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    meshcore:
      enabled: true
      hashtag: "#general"
    meshtastic:
      enabled: true
      channel_name: "general"
`
	got, err := Load(writeTemp(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b := got.Bridges[0]
	if !b.MeshCore.Enabled || !b.Meshtastic.Enabled {
		t.Errorf("expected both backends enabled: %+v", b)
	}
}

func TestLoad_GuildIDIsOptionalAndPassedThrough(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    guild_id: "999"
    meshcore:
      enabled: true
      hashtag: "#general"
`
	got, err := Load(writeTemp(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Bridges[0].GuildID != "999" {
		t.Errorf("GuildID = %q, want %q", got.Bridges[0].GuildID, "999")
	}
}

// configWithCoexistence is configWithLimits' counterpart for the top-level
// coexistence block.
func configWithCoexistence(extraFields string) string {
	return fmt.Sprintf(`
meshcore:
  host: 192.168.4.1
discord:
  bot_token: "abc123"
coexistence:
%s
bridges:
  - name: general
    discord_channel_id: "123"
    discord_webhook_url: "https://discord.com/api/webhooks/x/y"
    meshcore:
      enabled: true
      hashtag: "#general"
`, extraFields)
}

func TestCoexistence_Enabled_DefaultsTrueWhenUnset(t *testing.T) {
	if !(Coexistence{}).Enabled() {
		t.Error("Enabled() should default to true when avoid_simultaneous_tx is unset")
	}
}

func TestCoexistence_Enabled_RespectsExplicitFalse(t *testing.T) {
	f := false
	if (Coexistence{AvoidSimultaneousTX: &f}).Enabled() {
		t.Error("Enabled() should be false when avoid_simultaneous_tx: false is set")
	}
}

func TestCoexistence_Enabled_RespectsExplicitTrue(t *testing.T) {
	tr := true
	if !(Coexistence{AvoidSimultaneousTX: &tr}).Enabled() {
		t.Error("Enabled() should be true when avoid_simultaneous_tx: true is set")
	}
}

func TestCoexistence_GapDuration(t *testing.T) {
	got := Coexistence{MinGapMs: 250}.GapDuration()
	if got != 250*time.Millisecond {
		t.Errorf("GapDuration() = %v, want 250ms", got)
	}
}

func TestValidSenderFormat(t *testing.T) {
	for _, ok := range []string{"", "none", "short", "full"} {
		if err := validSenderFormat(ok); err != nil {
			t.Errorf("validSenderFormat(%q): %v", ok, err)
		}
	}
	if err := validSenderFormat("bogus"); err == nil {
		t.Error("expected an error for an unrecognised sender_format")
	}
}

func TestLoad_SenderFormatDefaultsToNone(t *testing.T) {
	cfg, err := Load(writeTemp(t, validMinimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SenderFormat != "none" {
		t.Errorf("SenderFormat = %q, want %q", cfg.SenderFormat, "none")
	}
}

func TestLoad_SenderFormatIsConfigurable(t *testing.T) {
	for _, v := range []string{"none", "short", "full"} {
		cfg := configWithSenderFormat(v)
		got, err := Load(writeTemp(t, cfg))
		if err != nil {
			t.Fatalf("Load(sender_format=%q): %v", v, err)
		}
		if got.SenderFormat != v {
			t.Errorf("SenderFormat = %q, want %q", got.SenderFormat, v)
		}
	}
}

func TestLoad_RejectsUnknownSenderFormat(t *testing.T) {
	cfg := configWithSenderFormat("bogus")
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "sender_format") {
		t.Fatalf("expected a sender_format error, got %v", err)
	}
}

func TestLoad_RejectsUnknownPerBridgeSenderFormat(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    sender_format: bogus
    meshcore:
      enabled: true
      hashtag: "#general"
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "sender_format") {
		t.Fatalf("expected a sender_format error, got %v", err)
	}
}

func TestBridge_ResolvedSenderFormat_UsesOverrideWhenSet(t *testing.T) {
	b := Bridge{SenderFormat: "full"}
	if got := b.ResolvedSenderFormat("none"); got != "full" {
		t.Errorf("ResolvedSenderFormat = %q, want %q", got, "full")
	}
}

func TestBridge_ResolvedSenderFormat_FallsBackToGlobal(t *testing.T) {
	b := Bridge{}
	if got := b.ResolvedSenderFormat("short"); got != "short" {
		t.Errorf("ResolvedSenderFormat = %q, want %q", got, "short")
	}
}

func TestLoad_CoexistenceDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, validMinimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Coexistence.Enabled() {
		t.Error("Coexistence.Enabled() should default to true")
	}
	if cfg.Coexistence.MinGapMs != 100 {
		t.Errorf("MinGapMs = %d, want 100", cfg.Coexistence.MinGapMs)
	}
}

func TestLoad_CoexistenceCanBeDisabled(t *testing.T) {
	cfg := configWithCoexistence("  avoid_simultaneous_tx: false")
	got, err := Load(writeTemp(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Coexistence.Enabled() {
		t.Error("expected avoid_simultaneous_tx: false to disable coexistence")
	}
}

func TestLoad_CoexistenceGapIsConfigurable(t *testing.T) {
	cfg := configWithCoexistence("  min_gap_ms: 500")
	got, err := Load(writeTemp(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Coexistence.MinGapMs != 500 {
		t.Errorf("MinGapMs = %d, want 500", got.Coexistence.MinGapMs)
	}
}

func TestLoad_RejectsNegativeMinGapMs(t *testing.T) {
	cfg := configWithCoexistence("  min_gap_ms: -1")
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "min_gap_ms") {
		t.Fatalf("expected min_gap_ms error, got %v", err)
	}
}

func TestLoad_LimitsDefaultsTo320(t *testing.T) {
	cfg, err := Load(writeTemp(t, validMinimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Limits.MaxMessageBytes != 320 {
		t.Errorf("MaxMessageBytes = %d, want 320", cfg.Limits.MaxMessageBytes)
	}
}

func TestLoad_LimitsIsConfigurable(t *testing.T) {
	cfg := configWithLimits("  max_message_bytes: 480")
	got, err := Load(writeTemp(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Limits.MaxMessageBytes != 480 {
		t.Errorf("MaxMessageBytes = %d, want 480", got.Limits.MaxMessageBytes)
	}
}

func TestLoad_RejectsNonPositiveMaxMessageBytes(t *testing.T) {
	// Note: 0 is indistinguishable from "unset" and resolves to the 320
	// default via applyDefaults, so this specifically exercises a negative
	// value, which YAML/Go can express but must still be rejected.
	cfg := configWithLimits("  max_message_bytes: -1")
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "max_message_bytes") {
		t.Fatalf("expected max_message_bytes error, got %v", err)
	}
}

func TestLoad_RejectsNegativePerBridgeMaxMessageBytes(t *testing.T) {
	cfg := `
meshcore:
  host: 1.2.3.4
discord:
  bot_token: abc
bridges:
  - name: general
    discord_channel_id: "1"
    discord_webhook_url: "https://x"
    max_message_bytes: -5
    meshcore:
      enabled: true
      hashtag: "#general"
`
	_, err := Load(writeTemp(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "max_message_bytes") {
		t.Fatalf("expected max_message_bytes error, got %v", err)
	}
}

func TestExampleConfig_ParsesWithPlaceholdersReplaced(t *testing.T) {
	// Sanity-check config.example.yaml's shape by loading it after swapping
	// in valid placeholder values, catching drift between the YAML schema
	// and this package's struct tags.
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatalf("reading config.example.yaml: %v", err)
	}
	replaced := strings.NewReplacer(
		"REPLACE_WITH_YOUR_BOT_TOKEN", "abc123",
		"REPLACE_WITH_DISCORD_CHANNEL_ID", "123456789012345678",
		"REPLACE_WITH_DISCORD_WEBHOOK_URL", "https://discord.com/api/webhooks/x/y",
	).Replace(string(data))

	path := writeTemp(t, replaced)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(config.example.yaml): %v", err)
	}
	if cfg.Meshcore.PathHashBytes != 3 {
		t.Errorf("PathHashBytes = %d, want 3", cfg.Meshcore.PathHashBytes)
	}
	if len(cfg.Bridges) < 1 {
		t.Errorf("unexpected bridges: %+v", cfg.Bridges)
	}
}
