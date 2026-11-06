package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M20: a storage move killed after the key flip left the source half deleted
// and the container bound to it, and the redelivered command was refused as
// "already on path". That refusal is now the signal to finish the move.
func TestAStorageMoveAlreadyOnTheTargetIsFinished(t *testing.T) {
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7"
	for _, tc := range []struct {
		name      string
		keyed     bool
		wantKept  bool
		wantError bool
	}{
		{"key names the target", true, false, true},
		{"found by scanning only", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newDiskGuardRedis(t)
			roots := []string{t.TempDir(), t.TempDir()}
			sm := NewStorageManager(strings.Join(roots, ","), rdb)
			for _, r := range roots {
				if err := os.MkdirAll(filepath.Join(r, uuid, "world"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.keyed {
				if err := sm.setRedisPath(uuid, roots[1]); err != nil {
					t.Fatal(err)
				}
			}
			err := sm.CheckMigration(uuid, roots[1])
			if got := errors.Is(err, errAlreadyOnPath); got != tc.wantError {
				t.Fatalf("CheckMigration = %v, want already-on-path %v", err, tc.wantError)
			}
			sm.dropStaleCopies(uuid)
			if _, err := os.Stat(filepath.Join(roots[0], uuid)); (err == nil) != tc.wantKept {
				t.Fatalf("leftover source copy present = %v, want %v", err == nil, tc.wantKept)
			}
			if _, err := os.Stat(filepath.Join(roots[1], uuid, "world")); err != nil {
				t.Fatalf("the live copy is gone: %v", err)
			}
		})
	}
}

func TestDataBindStale(t *testing.T) {
	for _, tc := range []struct {
		name  string
		binds []string
		want  bool
	}{
		{"bound to the server dir", []string{"/b/srv:/data"}, false},
		{"bound to the old path", []string{"/a/srv:/data"}, true},
		{"read-only option", []string{"/a/srv:/data:rw"}, true},
		{"no data bind", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dataBindStale(tc.binds, "/b/srv"); got != tc.want {
				t.Fatalf("dataBindStale(%v) = %v, want %v", tc.binds, got, tc.want)
			}
		})
	}
}
