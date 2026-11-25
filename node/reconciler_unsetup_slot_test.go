package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R53-01: create leaves mc_<uuid> with no command until setup runs, and the
// crash-restart pass started it as soon as Core's IntendOnline landed. The
// slot exits at once, and each try counted as a crash against the install.
func TestCrashRestartSkipsAServerNeverSetUp(t *testing.T) {
	const uuid = "53535353-0101-0101-0101-535353535353"
	for _, tc := range []struct {
		name   string
		active bool
		mkdir  bool
		want   bool
	}{
		{"created slot, setup still queued", false, true, true},
		{"set up", true, true, false},
		{"directory not made, left to the other guards", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, uuid)
			if tc.mkdir {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.active {
				if err := os.WriteFile(filepath.Join(dir, ".active_server"), []byte("a"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := neverSetUp(&StorageManager{paths: []string{root}}, uuid); got != tc.want {
				t.Errorf("neverSetUp = %v, want %v", got, tc.want)
			}
		})
	}
	rec, err := os.ReadFile("reconciler.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after(t, string(rec), "func StartReconciler("), "neverSetUp(") {
		t.Error("the crash-restart pass does not skip a server that was never set up")
	}
}

// R53-01: a streak counted before an install outlived it, so the fresh
// server's first-boot crash got fewer restarts or none. Every deliberate
// start clears reconcile_failed, and the streak goes with it.
func TestADeliberateStartResetsTheCrashStreak(t *testing.T) {
	const uuid = "53535353-0202-0202-0202-535353535353"
	tracker := map[string]*reconcileInfo{uuid: {crashCount: maxCrashRetries}, "other": {crashCount: 2}}
	takeCrashStreakReset(tracker, uuid)
	if tracker[uuid] == nil {
		t.Fatal("a streak was dropped with no deliberate start")
	}
	clearRunBlockers(context.Background(), newMiniRedis(t), uuid)
	takeCrashStreakReset(tracker, uuid)
	if tracker[uuid] != nil {
		t.Error("the crash streak outlived a deliberate start")
	}
	if tracker["other"] == nil {
		t.Error("another server's streak was dropped")
	}
	rec, err := os.ReadFile("reconciler.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after(t, string(rec), "func StartReconciler("), "takeCrashStreakReset(tracker, c.UUID)") {
		t.Error("the crash-restart pass never takes the reset")
	}
}
