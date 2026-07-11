package main

import (
	"context"
	"testing"
	"time"
)

func TestNextBackoff_Doubles(t *testing.T) {
	got := nextBackoff(time.Second)
	if got != 2*time.Second {
		t.Errorf("nextBackoff(1s) = %v, want 2s", got)
	}
}

func TestNextBackoff_CapsAtMax(t *testing.T) {
	got := nextBackoff(maxReconnectBackoff)
	if got != maxReconnectBackoff {
		t.Errorf("nextBackoff(max) = %v, want capped at %v", got, maxReconnectBackoff)
	}
}

func TestNextBackoff_CapsWhenDoublingWouldExceedMax(t *testing.T) {
	got := nextBackoff(maxReconnectBackoff - time.Millisecond)
	if got != maxReconnectBackoff {
		t.Errorf("nextBackoff(just under max) = %v, want capped at %v", got, maxReconnectBackoff)
	}
}

func TestSleepOrDone_ReturnsTrueAfterDuration(t *testing.T) {
	if !sleepOrDone(context.Background(), 10*time.Millisecond) {
		t.Error("sleepOrDone should return true when the duration elapses before cancellation")
	}
}

func TestSleepOrDone_ReturnsFalseOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepOrDone(ctx, time.Second) {
		t.Error("sleepOrDone should return false immediately when ctx is already cancelled")
	}
}

func TestSleepOrDone_CancelDuringWaitWinsOverLongDuration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if sleepOrDone(ctx, 10*time.Second) {
		t.Error("sleepOrDone should return false when ctx is cancelled before the duration elapses")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("sleepOrDone took %v, expected it to return promptly on cancellation", elapsed)
	}
}
