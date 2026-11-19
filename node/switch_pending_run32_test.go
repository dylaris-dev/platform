package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// R32-02: Core publishes desired_state "online" when it dispatches a switch,
// so a stopped server read to the reconciler as one it was meant to run - both
// before the node pulled the switch and after the node refused it, until Core
// put the intent back - and it started the OLD sub-server the owner had
// stopped. Core's undo record says the server was not meant to run; while the
// switch has not landed, it holds the server down.
func TestAPendingSwitchOfAStoppedServerHoldsTheOldSubServerDown(t *testing.T) {
	const uuid = "32323232-0202-0202-0202-323232323232"
	// R33-01: Core stamps the dispatch time; a record without one holds nothing.
	at := fmt.Sprint(time.Now().UnixNano())
	for _, tc := range []struct {
		name, undo, active string
		want               bool
	}{
		{"pending, was stopped", `{"target":"b","previous":"a","desiredState":"stopped","at":` + at + `}`, "a", true},
		{"pending, none active before", `{"target":"b","previous":"","desiredState":"stopped","at":` + at + `}`, "", true},
		{"landed", `{"target":"b","previous":"a","desiredState":"stopped"}`, "b", false},
		{"landed, other case", `{"target":"B","previous":"a","desiredState":"stopped"}`, "b", false},
		{"was running", `{"target":"b","previous":"a","desiredState":"online"}`, "a", false},
		{"record from an older Core", `{"target":"b","previous":"a"}`, "a", false},
		{"no record", "", "a", false},
		{"unreadable", `nope`, "a", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, uuid)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.active != "" {
				if err := os.WriteFile(filepath.Join(dir, ".active_server"), []byte(tc.active), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rdb := newMiniRedis(t)
			ctx := context.Background()
			if tc.undo != "" {
				rdb.Set(ctx, switchUndoKey(uuid), tc.undo, 0)
			}
			if got := restartBlocked(ctx, rdb, &StorageManager{paths: []string{root}}, uuid); got != tc.want {
				t.Errorf("restartBlocked = %v, want %v", got, tc.want)
			}
		})
	}
}

// R32-04: a switch that landed has nothing to undo, so the node removes Core's
// record and the hold above cannot outlive it. Source check, like the other
// switch_server branch checks: the branch needs a real Docker to succeed.
func TestALandedSwitchRemovesTheUndoRecord(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := between(t, string(b), "case \"switch_server\":", "case \"start\":")
	landed := after(t, body, "writeServerRootFile(serverPath, \".active_server\"")
	if !strings.Contains(landed, "switchUndoKey(") {
		t.Error("a landed switch leaves Core's undo record behind")
	}
}
