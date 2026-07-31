package meshcore

import (
	"math"
	"sync"
	"time"
)

// LoraTimeOnAirMs returns the LoRa time-on-air in milliseconds for a packet
// of payloadBytes at the given radio settings (SX127x formula, explicit
// header, CRC on, MeshCore's 16-symbol preamble). crDenom is MeshCore's
// coding-rate field (5–8 → 4/5…4/8). Returns 0 for nonsense inputs.
func LoraTimeOnAirMs(payloadBytes int, sf uint8, bwHz float64, crDenom uint8) float64 {
	if payloadBytes <= 0 || sf < 5 || sf > 12 || bwHz <= 0 {
		return 0
	}
	cr := int(crDenom)
	if cr < 5 {
		cr = 5
	}
	if cr > 8 {
		cr = 8
	}
	cr -= 4 // 1..4
	tSymMs := math.Exp2(float64(sf)) / bwHz * 1000.0
	de := 0
	if tSymMs > 16.0 { // low data-rate optimization (standard rule)
		de = 1
	}
	const preambleSymbols = 16.0
	preambleMs := (preambleSymbols + 4.25) * tSymMs
	numerator := float64(8*payloadBytes - 4*int(sf) + 28 + 16)
	payloadSymbols := 8.0
	if extra := math.Ceil(numerator/float64(4*(int(sf)-2*de))) * float64(cr+4); extra > 0 {
		payloadSymbols += extra
	}
	return preambleMs + payloadSymbols*tSymMs
}

// ActivityMonitor tracks recent channel airtime (heard frames + our own
// transmissions) so senders can listen before talking. Thread-safe.
type ActivityMonitor struct {
	mu     sync.Mutex
	window time.Duration
	events []airEvent // oldest first
}

type airEvent struct {
	end   time.Time
	airMs float64
}

// NewActivityMonitor returns a monitor with a 30 s utilization window.
func NewActivityMonitor() *ActivityMonitor {
	return &ActivityMonitor{window: 30 * time.Second}
}

// Record notes a frame that finished at "at" occupying airMs of airtime.
func (m *ActivityMonitor) Record(at time.Time, airMs float64) {
	if airMs <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, airEvent{end: at, airMs: airMs})
	cutoff := at.Add(-2 * m.window)
	i := 0
	for i < len(m.events) && m.events[i].end.Before(cutoff) {
		i++
	}
	m.events = m.events[i:]
}

// RecordFrame computes the airtime for a frame of frameBytes at the given
// radio settings and records it ending at "at".
func (m *ActivityMonitor) RecordFrame(at time.Time, frameBytes int, sf uint8, bwHz float64, crDenom uint8) {
	m.Record(at, LoraTimeOnAirMs(frameBytes, sf, bwHz, crDenom))
}

// Utilization returns the fraction (0–1) of the sliding window before "now"
// occupied by recorded frames.
func (m *ActivityMonitor) Utilization(now time.Time) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	windowMs := float64(m.window.Milliseconds())
	start := now.Add(-m.window)
	busy := 0.0
	for _, e := range m.events {
		frameStart := e.end.Add(-time.Duration(e.airMs) * time.Millisecond)
		overlapStart := frameStart
		if overlapStart.Before(start) {
			overlapStart = start
		}
		overlapEnd := e.end
		if overlapEnd.After(now) {
			overlapEnd = now
		}
		if overlapEnd.After(overlapStart) {
			busy += float64(overlapEnd.Sub(overlapStart).Milliseconds())
		}
	}
	u := busy / windowMs
	if u < 0 {
		return 0
	}
	if u > 1 {
		return 1
	}
	return u
}

// QuietFor returns how long the channel has been quiet before "now"
// (a very large duration when nothing has ever been recorded).
func (m *ActivityMonitor) QuietFor(now time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.events) == 0 {
		return time.Duration(math.MaxInt64)
	}
	d := now.Sub(m.events[len(m.events)-1].end)
	if d < 0 {
		return 0
	}
	return d
}

// BusyNow reports whether the channel should be treated as busy: a frame
// ended less than 1.2 s ago (someone is actively talking) OR window
// utilization exceeds 25%.
func (m *ActivityMonitor) BusyNow(now time.Time) bool {
	if m.QuietFor(now) < 1200*time.Millisecond {
		return true
	}
	return m.Utilization(now) > 0.25
}
