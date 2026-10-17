package main

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

// A stop sent in the same second a server printed "Done" got its save-all
// refused ("getLevel() is null"): the world was not loaded yet. A container
// that only just started skips the save; every other one keeps it.
func TestJustStarted(t *testing.T) {
	now := time.Date(2026, 10, 17, 20, 15, 37, 0, time.UTC)
	tests := []struct {
		name string
		st   *container.State
		want bool
	}{
		{"started a second ago", &container.State{StartedAt: now.Add(-time.Second).Format(time.RFC3339Nano)}, true},
		{"started just under the minimum", &container.State{StartedAt: now.Add(-saveAllMinUptime + time.Second).Format(time.RFC3339Nano)}, true},
		{"running for the minimum", &container.State{StartedAt: now.Add(-saveAllMinUptime).Format(time.RFC3339Nano)}, false},
		{"running for hours", &container.State{StartedAt: now.Add(-5 * time.Hour).Format(time.RFC3339Nano)}, false},
		{"unreadable start time keeps the save", &container.State{StartedAt: "garbage"}, false},
		{"no state keeps the save", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := justStarted(tt.st, now); got != tt.want {
				t.Errorf("justStarted = %v, want %v", got, tt.want)
			}
		})
	}
}
