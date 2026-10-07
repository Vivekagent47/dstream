package main

import (
	"testing"
	"time"
)

func TestWatchdogHealthyAfterBeat(t *testing.T) {
	wd := newWatchdog()
	wd.beat()
	if !wd.healthy(50 * time.Millisecond) {
		t.Fatal("fresh beat must read healthy")
	}
}

func TestWatchdogUnhealthyAfterStall(t *testing.T) {
	wd := newWatchdog()
	wd.beat()
	time.Sleep(20 * time.Millisecond)
	if wd.healthy(5 * time.Millisecond) {
		t.Fatal("beat older than the stall window must read unhealthy")
	}
}

// Review Focus: a busy-but-advancing loop keeps beating, so it stays healthy.
func TestWatchdogStaysHealthyWhileAdvancing(t *testing.T) {
	wd := newWatchdog()
	deadline := time.Now().Add(60 * time.Millisecond)
	for time.Now().Before(deadline) {
		wd.beat()
		if !wd.healthy(30 * time.Millisecond) {
			t.Fatal("an advancing loop must never trip the watchdog")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
