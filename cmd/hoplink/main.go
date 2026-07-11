// Command hoplink bridges Discord text channels with a MeshCore mesh
// and/or a Meshtastic mesh. MeshCore channels are relayed via raw RF packet
// construction so Discord display names appear as the mesh sender and mesh
// node names appear as Discord webhook usernames; Meshtastic channels are
// relayed via its standard client API (see internal/meshtastic).
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/hectospark/hoplink/internal/bridge"
	"github.com/hectospark/hoplink/internal/config"
	"github.com/hectospark/hoplink/internal/discord"
	"github.com/hectospark/hoplink/internal/meshcore"
	"github.com/hectospark/hoplink/internal/meshtastic"
)

const maxReconnectBackoff = 30 * time.Second

// version is set at build time via -ldflags "-X main.version=...":
// go builds use "dev"; released binaries/images get their git tag.
var version = "dev"

func logf(format string, args ...any) {
	log.Printf("[hoplink] "+format, args...)
}

func fatalf(format string, args ...any) {
	log.Fatalf("[hoplink] "+format, args...)
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to hoplink config file")
	flag.Parse()

	logf("version %s starting", version)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatalf("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	preferDisplayName, err := cfg.Discord.PreferDisplayName()
	if err != nil {
		fatalf("%v", err) // already validated in config.Load; a real bug if this fires
	}
	bot, err := discord.NewBot(cfg.Discord.BotToken, preferDisplayName)
	if err != nil {
		fatalf("creating discord bot: %v", err)
	}
	if err := bot.Open(); err != nil {
		fatalf("opening discord gateway: %v", err)
	}
	defer func() {
		if err := bot.Close(); err != nil {
			logf("closing discord gateway: %v", err)
		}
	}()

	br, err := bridge.New(cfg, bot)
	if err != nil {
		// Config is already validated in config.Load; reaching here means a
		// real bug (e.g. a channel secret that stopped resolving).
		fatalf("building bridge: %v", err)
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		br.RunHousekeeping(ctx)
	}()

	// Each backend reconnects independently: one dropping never disturbs
	// the other, and a backend with no bridges using it is never dialed.
	if cfg.Meshcore.Host != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runMeshcoreWithReconnect(ctx, cfg, br)
		}()
	}
	if cfg.Meshtastic.Configured() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runMeshtasticWithReconnect(ctx, cfg, br)
		}()
	}

	wg.Wait()
	logf("shutting down")
}

// runMeshcoreWithReconnect owns the MeshCore TCP connection's lifecycle:
// connect, run it against br until the connection drops or ctx is
// cancelled, then reconnect with exponential backoff.
func runMeshcoreWithReconnect(ctx context.Context, cfg *config.Config, br *bridge.Bridge) {
	backoff := time.Second

	for ctx.Err() == nil {
		session, info, err := meshcore.Dial(cfg.Meshcore.Addr(), cfg.Meshcore.AppName)
		if err != nil {
			logf("connecting to meshcore %s: %v (retrying in %s)", cfg.Meshcore.Addr(), err, backoff)
			if !sleepOrDone(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		logf("connected to MeshCore radio %q at %s", info.Name, cfg.Meshcore.Addr())
		backoff = time.Second

		runErr := br.RunMeshcore(ctx, session)
		_ = session.Close()
		if ctx.Err() != nil {
			return
		}
		logf("meshcore session ended: %v (reconnecting in %s)", runErr, backoff)
		if !sleepOrDone(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// runMeshtasticWithReconnect is runMeshcoreWithReconnect's counterpart for
// the Meshtastic backend.
func runMeshtasticWithReconnect(ctx context.Context, cfg *config.Config, br *bridge.Bridge) {
	backoff := time.Second

	for ctx.Err() == nil {
		session, err := meshtastic.Dial(cfg.Meshtastic.Addr())
		if err != nil {
			logf("connecting to meshtastic %s: %v (retrying in %s)", cfg.Meshtastic.Addr(), err, backoff)
			if !sleepOrDone(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		logf("connected to Meshtastic device at %s (node #%d)", cfg.Meshtastic.Addr(), session.MyNodeNum())
		backoff = time.Second

		runErr := br.RunMeshtastic(ctx, session)
		_ = session.Close()
		if ctx.Err() != nil {
			return
		}
		logf("meshtastic session ended: %v (reconnecting in %s)", runErr, backoff)
		if !sleepOrDone(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > maxReconnectBackoff {
		next = maxReconnectBackoff
	}
	return next
}
