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

// R34-01/03/05/07: the reconciler no longer reads Core's dispatch records.
// Their age was judged by the node's own clock against Core's stamp, so a
// drifted node held a stopped server's crash restart off for hours or never,
// and the bound ran out while a command still queued. Core puts a refused
// switch's or setup's intent back instead (RestoreSetupBefore,
// consumeSwitchSubFailures); the window until then is accepted
// (SwitchSubServer in core). Whatever a record says, it holds nothing.
func TestADispatchRecordNeverHoldsTheReconcilerOff(t *testing.T) {
	const uuid = "34343434-0101-0101-0101-343434343434"
	now := time.Now()
	for _, tc := range []struct {
		name, key, record string
	}{
		{"pending switch", switchUndoKey(uuid), fmt.Sprintf(`{"target":"b","previous":"a","desiredState":"stopped","at":%d}`, now.UnixNano())},
		{"switch stamped ahead of a slow node clock", switchUndoKey(uuid), fmt.Sprintf(`{"target":"b","previous":"a","desiredState":"stopped","at":%d}`, now.Add(time.Hour).UnixNano())},
		{"pending setup", setupBeforeKey(uuid), fmt.Sprintf(`{"Target":"b","DesiredState":"stopped","At":%d}`, now.UnixNano())},
		{"setup stamped ahead of a slow node clock", setupBeforeKey(uuid), fmt.Sprintf(`{"Target":"b","DesiredState":"stopped","At":%d}`, now.Add(time.Hour).UnixNano())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, uuid)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".active_server"), []byte("a"), 0o644); err != nil {
				t.Fatal(err)
			}
			rdb := newMiniRedis(t)
			ctx := context.Background()
			rdb.Set(ctx, tc.key, tc.record, time.Hour)
			if restartBlocked(ctx, rdb, &StorageManager{paths: []string{root}}, uuid) {
				t.Error("a dispatch record held the reconciler off")
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
