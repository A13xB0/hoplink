package meshcore

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestLoraTimeOnAirMs_SF7(t *testing.T) {
	ms := LoraTimeOnAirMs(50, 7, 125000, 5)
	if ms < 80 || ms > 130 {
		t.Errorf("SF7/125k/4-5 50B = %.1fms, want ~80-130ms", ms)
	}
}

func TestLoraTimeOnAirMs_SF12MuchSlower(t *testing.T) {
	sf7 := LoraTimeOnAirMs(50, 7, 125000, 5)
	sf12 := LoraTimeOnAirMs(50, 12, 125000, 5)
	if sf12/sf7 < 15 {
		t.Errorf("SF12/SF7 ratio = %.1f, want > 15", sf12/sf7)
	}
	if sf12 < 1500 {
		t.Errorf("SF12 50B = %.0fms, want > 1500ms (published tables)", sf12)
	}
}

func TestLoraTimeOnAirMs_BandwidthProportional(t *testing.T) {
	bw125 := LoraTimeOnAirMs(30, 9, 125000, 5)
	bw250 := LoraTimeOnAirMs(30, 9, 250000, 5)
	ratio := bw125 / bw250
	if ratio < 1.99 || ratio > 2.01 {
		t.Errorf("125k/250k ratio = %.3f, want 2.0", ratio)
	}
}

func TestLoraTimeOnAirMs_GarbageReturnsZero(t *testing.T) {
	if LoraTimeOnAirMs(0, 7, 125000, 5) != 0 {
		t.Error("payload 0 should return 0")
	}
	if LoraTimeOnAirMs(10, 99, 125000, 5) != 0 {
		t.Error("SF 99 should return 0")
	}
}

func TestActivityMonitor_EmptyIsIdle(t *testing.T) {
	m := NewActivityMonitor()
	if m.Utilization(t0) != 0 {
		t.Error("empty monitor should have 0 utilization")
	}
	if m.BusyNow(t0) {
		t.Error("empty monitor should not be busy")
	}
}

func TestActivityMonitor_RecentFrameIsBusy(t *testing.T) {
	m := NewActivityMonitor()
	m.Record(t0, 500)
	if !m.BusyNow(t0.Add(300 * time.Millisecond)) {
		t.Error("channel should be busy 300ms after a frame ended")
	}
	if m.BusyNow(t0.Add(10 * time.Second)) {
		t.Error("channel should be idle 10s after one small frame")
	}
}

func TestActivityMonitor_UtilizationWindow(t *testing.T) {
	m := NewActivityMonitor() // 30s window
	// 6 s of air in the 30 s window = 20%.
	m.Record(t0, 6000)
	u := m.Utilization(t0.Add(2 * time.Second))
	if u < 0.19 || u > 0.21 {
		t.Errorf("utilization = %.3f, want ~0.20", u)
	}
}

func TestActivityMonitor_SustainedTrafficBusyEvenWhenQuiet(t *testing.T) {
	m := NewActivityMonitor()
	// 4 × 3 s frames in 30 s = 40%; last ended 4 s ago (> quiet gap).
	for i := 0; i < 4; i++ {
		m.Record(t0.Add(time.Duration(i*6)*time.Second), 3000)
	}
	now := t0.Add(22 * time.Second)
	if m.QuietFor(now) < 1200*time.Millisecond {
		t.Fatal("test setup: expected quiet gap to have passed")
	}
	if !m.BusyNow(now) {
		t.Error("40%% utilization should read busy even when momentarily quiet")
	}
}

func TestActivityMonitor_OldFramesAgeOut(t *testing.T) {
	m := NewActivityMonitor()
	m.Record(t0, 5000)
	if u := m.Utilization(t0.Add(40 * time.Second)); u > 0.001 {
		t.Errorf("utilization after window passed = %.3f, want 0", u)
	}
}
