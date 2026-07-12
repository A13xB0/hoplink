package bridge

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hectospark/hoplink/internal/config"
	"github.com/hectospark/hoplink/internal/discord"
	"github.com/hectospark/hoplink/internal/meshcore"
)

func TestBridge_WithTxGuard_SerializesConcurrentSends(t *testing.T) {
	b := &Bridge{txGuardEnabled: true, txGuardGap: 20 * time.Millisecond}

	var mu sync.Mutex
	active, maxActive := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.withTxGuard(func() error {
				mu.Lock()
				active++
				if active > maxActive {
					maxActive = active
				}
				mu.Unlock()

				time.Sleep(30 * time.Millisecond)

				mu.Lock()
				active--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	if maxActive > 1 {
		t.Errorf("expected sends to never overlap while the guard is enabled, but saw %d concurrent", maxActive)
	}
}

func TestBridge_WithTxGuard_DisabledAllowsConcurrentSends(t *testing.T) {
	b := &Bridge{txGuardEnabled: false}

	var mu sync.Mutex
	active, maxActive := 0, 0
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.withTxGuard(func() error {
				mu.Lock()
				active++
				if active > maxActive {
					maxActive = active
				}
				mu.Unlock()
				<-release
				return nil
			})
		}()
	}

	// Give both goroutines a chance to enter concurrently before releasing them.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if maxActive < 2 {
		t.Errorf("expected sends to run concurrently when the guard is disabled, maxActive=%d", maxActive)
	}
}

func TestBridge_WithTxGuard_EnforcesGapAfterSend(t *testing.T) {
	b := &Bridge{txGuardEnabled: true, txGuardGap: 100 * time.Millisecond}

	start := time.Now()
	_ = b.withTxGuard(func() error { return nil })
	elapsed := time.Since(start)

	if elapsed < 100*time.Millisecond {
		t.Errorf("expected withTxGuard to hold the configured gap after sending, elapsed=%v", elapsed)
	}
}

func TestBridge_WithTxGuard_NoGapWhenZero(t *testing.T) {
	b := &Bridge{txGuardEnabled: true, txGuardGap: 0}

	start := time.Now()
	_ = b.withTxGuard(func() error { return nil })
	elapsed := time.Since(start)

	if elapsed > 20*time.Millisecond {
		t.Errorf("expected no meaningful delay with a zero gap, elapsed=%v", elapsed)
	}
}

func TestBridge_WithTxGuard_PropagatesSendError(t *testing.T) {
	b := &Bridge{txGuardEnabled: true}
	wantErr := errors.New("boom")
	if err := b.withTxGuard(func() error { return wantErr }); err != wantErr {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}

func TestNew_WiresDebugFromConfig(t *testing.T) {
	cfg := &config.Config{
		Meshcore: config.Meshcore{Host: "1.2.3.4", PathHashBytes: 3},
		Discord:  config.Discord{BotToken: "abc"},
		Limits:   config.Limits{MaxMessageBytes: 320},
		Debug:    true,
		Bridges: []config.Bridge{{
			Name:              "general",
			DiscordChannelID:  "1",
			DiscordWebhookURL: "https://discord.com/api/webhooks/x/y",
			MeshCore:          config.BridgeMeshCore{Enabled: true, Hashtag: "#general"},
		}},
	}
	bot, err := discord.NewBot("fake-token", true)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	b, err := New(cfg, bot)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !b.debug {
		t.Error("debug should be true when config.Debug is true")
	}
}

func TestNew_WiresCoexistenceFromConfig(t *testing.T) {
	falseVal := false
	cfg := &config.Config{
		Meshcore: config.Meshcore{Host: "1.2.3.4", PathHashBytes: 3},
		Discord:  config.Discord{BotToken: "abc"},
		Limits:   config.Limits{MaxMessageBytes: 320},
		Coexistence: config.Coexistence{
			AvoidSimultaneousTX: &falseVal,
			MinGapMs:            250,
		},
		Bridges: []config.Bridge{{
			Name:              "general",
			DiscordChannelID:  "1",
			DiscordWebhookURL: "https://discord.com/api/webhooks/x/y",
			MeshCore:          config.BridgeMeshCore{Enabled: true, Hashtag: "#general"},
		}},
	}
	bot, err := discord.NewBot("fake-token", true)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	b, err := New(cfg, bot)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.txGuardEnabled {
		t.Error("txGuardEnabled should be false when avoid_simultaneous_tx: false")
	}
	if b.txGuardGap != 250*time.Millisecond {
		t.Errorf("txGuardGap = %v, want 250ms", b.txGuardGap)
	}
}

func TestNew_WiresPerBridgeFloodScopeOverride(t *testing.T) {
	cfg := &config.Config{
		Meshcore: config.Meshcore{Host: "1.2.3.4", PathHashBytes: 3, FloodScope: "globalregion"},
		Discord:  config.Discord{BotToken: "abc"},
		Limits:   config.Limits{MaxMessageBytes: 320},
		Bridges: []config.Bridge{
			{
				Name:              "default-scope",
				DiscordChannelID:  "1",
				DiscordWebhookURL: "https://discord.com/api/webhooks/x/y",
				MeshCore:          config.BridgeMeshCore{Enabled: true, Hashtag: "#a"},
			},
			{
				Name:              "override-scope",
				DiscordChannelID:  "2",
				DiscordWebhookURL: "https://discord.com/api/webhooks/x/y",
				MeshCore:          config.BridgeMeshCore{Enabled: true, Hashtag: "#b", FloodScope: "bridgeregion"},
			},
		},
	}
	bot, err := discord.NewBot("fake-token", true)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	b, err := New(cfg, bot)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	globalKey := meshcore.FloodScopeKey("globalregion")
	overrideKey := meshcore.FloodScopeKey("bridgeregion")

	mDefault := b.byChan["1"]
	if string(mDefault.scopeKey) != string(globalKey) {
		t.Errorf("bridge %q scopeKey = %x, want the global default %x", mDefault.cfg.Name, mDefault.scopeKey, globalKey)
	}
	mOverride := b.byChan["2"]
	if string(mOverride.scopeKey) != string(overrideKey) {
		t.Errorf("bridge %q scopeKey = %x, want its own override %x", mOverride.cfg.Name, mOverride.scopeKey, overrideKey)
	}
}

func TestNew_SkipsDisabledBridges(t *testing.T) {
	falseVal := false
	cfg := &config.Config{
		Meshcore: config.Meshcore{Host: "1.2.3.4", PathHashBytes: 3},
		Discord:  config.Discord{BotToken: "abc"},
		Limits:   config.Limits{MaxMessageBytes: 320},
		Bridges: []config.Bridge{
			{
				Name:              "active",
				DiscordChannelID:  "1",
				DiscordWebhookURL: "https://discord.com/api/webhooks/x/y",
				MeshCore:          config.BridgeMeshCore{Enabled: true, Hashtag: "#a"},
			},
			{
				Name:    "wip",
				Enabled: &falseVal,
				// Deliberately incomplete: no secret source, no webhook —
				// must never be built/validated, only skipped.
				DiscordChannelID: "2",
				MeshCore:         config.BridgeMeshCore{Enabled: true},
			},
		},
	}
	bot, err := discord.NewBot("fake-token", true)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	b, err := New(cfg, bot)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(b.byName) != 1 {
		t.Fatalf("byName should only contain the enabled bridge, got %d entries", len(b.byName))
	}
	if b.byName[0].cfg.Name != "active" {
		t.Errorf("byName[0].cfg.Name = %q, want %q", b.byName[0].cfg.Name, "active")
	}
	if _, ok := b.byChan["2"]; ok {
		t.Error("byChan should not contain an entry for the disabled bridge's channel")
	}
}

func TestNew_WithoutDiscord_BuildsMeshOnlyBridge(t *testing.T) {
	cfg := &config.Config{
		Meshcore:   config.Meshcore{Host: "1.2.3.4", PathHashBytes: 3},
		Meshtastic: config.Meshtastic{Host: "5.6.7.8"},
		Limits:     config.Limits{MaxMessageBytes: 320},
		Bridges: []config.Bridge{{
			Name:       "general",
			MeshCore:   config.BridgeMeshCore{Enabled: true, Hashtag: "#general"},
			Meshtastic: config.BridgeMeshtastic{Enabled: true, ChannelName: "LongFast"},
		}},
	}

	b, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(b.byChan) != 0 {
		t.Errorf("byChan should be empty for a bridge with no Discord side, got %d entries", len(b.byChan))
	}
	if len(b.byName) != 1 {
		t.Fatalf("byName should have 1 mapping, got %d", len(b.byName))
	}
	m := b.byName[0]
	if m.discordEnabled {
		t.Error("discordEnabled should be false")
	}
	if m.webhook != nil {
		t.Error("webhook should be nil when this bridge has no Discord side")
	}
	if b.notify != nil {
		t.Error("notify should be nil when New is called with a nil bot")
	}
}

func TestNew_CoexistenceDefaultsToEnabled(t *testing.T) {
	cfg := &config.Config{
		Meshcore: config.Meshcore{Host: "1.2.3.4", PathHashBytes: 3},
		Discord:  config.Discord{BotToken: "abc"},
		Limits:   config.Limits{MaxMessageBytes: 320},
		Coexistence: config.Coexistence{
			MinGapMs: 100,
		},
		Bridges: []config.Bridge{{
			Name:              "general",
			DiscordChannelID:  "1",
			DiscordWebhookURL: "https://discord.com/api/webhooks/x/y",
			MeshCore:          config.BridgeMeshCore{Enabled: true, Hashtag: "#general"},
		}},
	}
	bot, err := discord.NewBot("fake-token", true)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	b, err := New(cfg, bot)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !b.txGuardEnabled {
		t.Error("txGuardEnabled should default to true (AvoidSimultaneousTX unset)")
	}
}
