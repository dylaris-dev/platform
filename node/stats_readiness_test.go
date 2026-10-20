package main

import (
	"testing"
	"time"
)

// A container that runs is not a server that is up: it reports "starting"
// until the first ping answer since its start, and "online" only after it.
func TestStartReadiness(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	r := &startReadiness{started: t0}
	if got := r.status(t0.Add(time.Second)); got != "starting" {
		t.Fatalf("before any ping: %q, want starting", got)
	}
	r.pinged(false)
	if got := r.status(t0.Add(30 * time.Second)); got != "starting" {
		t.Fatalf("after a failed ping: %q, want starting", got)
	}
	r.pinged(true)
	if got := r.status(t0.Add(31 * time.Second)); got != "online" {
		t.Fatalf("after the first answer: %q, want online", got)
	}
	r.pinged(false)
	if got := r.status(t0.Add(time.Minute)); got != "online" {
		t.Fatalf("a later failed ping took readiness back: %q", got)
	}

	// The same start time again is not a restart.
	r.observeStart(t0, nil)
	if got := r.status(t0.Add(time.Minute)); got != "online" {
		t.Fatalf("re-observing the same start reset readiness: %q", got)
	}
	// A new start time is: starting again until the next answer.
	restart := t0.Add(5 * time.Minute)
	r.observeStart(restart, nil)
	if got := r.status(restart.Add(time.Second)); got != "starting" {
		t.Fatalf("after a restart: %q, want starting", got)
	}
	r.observeStart(time.Time{}, nil)
	if got := r.status(restart.Add(time.Second)); got != "starting" {
		t.Fatalf("an unreadable start time changed readiness: %q", got)
	}

	// A server that never answers is not stuck in starting forever.
	if got := r.status(restart.Add(readinessBound - time.Second)); got != "starting" {
		t.Fatalf("just before the bound: %q, want starting", got)
	}
	if got := r.status(restart.Add(readinessBound)); got != "online" {
		t.Fatalf("at the bound: %q, want online", got)
	}
	// "starting" turns players away at the edge, so the bound stays short.
	if got := r.status(restart.Add(5 * time.Minute)); got != "online" {
		t.Fatalf("five minutes after the start: %q, want online", got)
	}

	// A node restart finds a server that has been up for hours: online at once.
	old := &startReadiness{started: t0}
	old.observeStart(t0.Add(-3*time.Hour), nil)
	if got := old.status(t0); got != "online" {
		t.Fatalf("long-running container after a node restart: %q, want online", got)
	}
}

// enable-status=false: the server never answers a ping, so it is ready at once
// instead of turning players away for readinessBound. The file is read once
// per start, not on every inspect.
func TestStartReadinessWithThePingDisabled(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	reads := 0
	disabled := func() bool { reads++; return true }
	r := &startReadiness{started: t0.Add(-time.Second)}
	r.observeStart(t0, disabled)
	if got := r.status(t0.Add(time.Second)); got != "online" {
		t.Fatalf("ping disabled: %q, want online at once", got)
	}
	r.observeStart(t0, disabled)
	r.observeStart(t0, disabled)
	if reads != 1 {
		t.Fatalf("properties read %d times for one start, want 1", reads)
	}
	r.observeStart(t0.Add(time.Minute), disabled)
	if reads != 2 {
		t.Fatalf("properties not read again on a new start (%d reads)", reads)
	}

	enabled := &startReadiness{started: t0.Add(-time.Second)}
	enabled.observeStart(t0, func() bool { return false })
	if got := enabled.status(t0.Add(time.Second)); got != "starting" {
		t.Fatalf("ping enabled: %q, want starting until it answers", got)
	}
}
