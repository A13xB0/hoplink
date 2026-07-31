package bridge

import (
	"math/rand"
	"sync"
	"time"

	"github.com/hectospark/hoplink/internal/meshcore"
)

// Congestion-aware TX pacing for the MeshCore side: every raw-log frame the
// radio hears (and every transmission hoplink makes) feeds an airtime
// monitor; transmitMeshcore then listens before talking — deferring while
// the channel is busy (bounded) and keeping a minimum gap between hoplink's
// own transmissions, so relaying a lively Discord conversation can't
// machine-gun the mesh. Enabled by meshcore.busy_wait (default on).

type pacingState struct {
	mu        sync.Mutex
	monitor   *meshcore.ActivityMonitor
	lastOwnTx time.Time
	// Radio params from the session handshake (needed for time-on-air).
	sf      uint8
	bwHz    float64
	crDenom uint8
}

// ownTxOverheadBytes approximates the RF envelope around a text chunk
// (header + transport codes + path byte + crypto).
const ownTxOverheadBytes = 24

// SetMeshcoreRadioParams stores the radio settings from the session
// handshake so heard/sent frames can be converted to airtime. Called by the
// connection runner before RunMeshcore.
func (b *Bridge) SetMeshcoreRadioParams(info meshcore.SelfInfo) {
	b.pacing.mu.Lock()
	defer b.pacing.mu.Unlock()
	b.pacing.sf = info.RadioSF
	b.pacing.bwHz = info.RadioBW * 1000.0 // SelfInfo carries kHz
	b.pacing.crDenom = info.RadioCR
}

// recordHeardRfFrame feeds one heard raw-log frame into the monitor.
func (b *Bridge) recordHeardRfFrame(rawLen int) {
	b.pacing.mu.Lock()
	sf, bw, cr := b.pacing.sf, b.pacing.bwHz, b.pacing.crDenom
	b.pacing.mu.Unlock()
	if sf == 0 || bw <= 0 || rawLen <= 0 {
		return
	}
	b.pacing.monitor.RecordFrame(time.Now(), rawLen, sf, bw, cr)
}

// noteOwnTx records one of hoplink's own transmissions.
func (b *Bridge) noteOwnTx(payloadTextBytes int) {
	b.pacing.mu.Lock()
	sf, bw, cr := b.pacing.sf, b.pacing.bwHz, b.pacing.crDenom
	b.pacing.lastOwnTx = time.Now()
	b.pacing.mu.Unlock()
	if sf == 0 || bw <= 0 {
		return
	}
	b.pacing.monitor.RecordFrame(
		time.Now(), payloadTextBytes+ownTxOverheadBytes, sf, bw, cr)
}

// waitForQuietAir blocks until the channel is quiet AND we're at least
// minTxSpacing past our own last transmission — or until maxDefer elapses
// (a message is never held hostage; the firmware still does its own CAD).
// Returns how long it actually deferred. No-op when busy_wait is off.
func (b *Bridge) waitForQuietAir(maxDefer time.Duration, tag string) time.Duration {
	if !b.busyWaitEnabled {
		return 0
	}
	start := time.Now()
	logged := false
	for time.Since(start) < maxDefer {
		now := time.Now()
		b.pacing.mu.Lock()
		sinceOwn := now.Sub(b.pacing.lastOwnTx)
		b.pacing.mu.Unlock()
		busy := b.pacing.monitor.BusyNow(now)
		if !busy && sinceOwn >= b.minTxSpacing {
			break
		}
		if !logged {
			logged = true
			if busy {
				logfMeshcore("TX %s: deferring — channel busy (%d%% util)",
					tag, int(b.pacing.monitor.Utilization(now)*100))
			} else {
				logfMeshcore("TX %s: deferring — own-TX spacing", tag)
			}
		}
		select {
		case <-b.closedCh:
			return time.Since(start)
		default:
		}
		time.Sleep(time.Duration(350+rand.Intn(300)) * time.Millisecond)
	}
	deferred := time.Since(start)
	if logged {
		logfMeshcore("TX %s: proceeding after %s defer", tag, deferred.Round(100*time.Millisecond))
	}
	return deferred
}
