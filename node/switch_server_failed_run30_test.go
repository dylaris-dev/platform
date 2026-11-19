package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// R30-01/R30-04/R30-03: a switch to a directory with nothing runnable in it
// gave up before touching the container, wrote "stopped" over the server that
// kept running, and told Core nothing, so Core stayed on the target for good.
func TestRefusedSwitchReportsAndKeepsTheRunningStatus(t *testing.T) {
	const uuid = "30303030-0101-0101-0101-303030303030"
	for _, tc := range []struct {
		name, wd   string // wd "" = no container
		wantStatus string
	}{
		{"old sub-server still running", "/data/a", "online"},
		{"no container", "", "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, uuid)
			for _, sub := range []string{"a", "b"} {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, ".active_server"), []byte("a"), 0o644); err != nil {
				t.Fatal(err)
			}
			rdb := newMiniRedis(t)
			cmd := NodeCommand{Action: "switch_server"}
			cmd.Config.UUID = uuid
			cmd.Config.ActiveSubServer = "b"
			processCommand(context.Background(), cmd, "", rdb, dockerWithWorkingDir(t, tc.wd), "node-a", nil, &StorageManager{paths: []string{root}})

			ctx := context.Background()
			if got, _ := rdb.Get(ctx, fmt.Sprintf("dylaris:server:%s:status", uuid)).Result(); got != tc.wantStatus {
				t.Errorf("status = %q, want %q", got, tc.wantStatus)
			}
			raw, err := rdb.Get(ctx, fmt.Sprintf("dylaris:server:%s:switch_failed", uuid)).Result()
			if err != nil {
				t.Fatalf("no switch_failed report: %v", err)
			}
			var rep map[string]string
			if err := json.Unmarshal([]byte(raw), &rep); err != nil {
				t.Fatal(err)
			}
			if rep["target"] != "b" || rep["previous"] != "a" || rep["reason"] == "" {
				t.Errorf("report = %v, want target b, previous a and a reason", rep)
			}
			if ttl := rdb.TTL(ctx, fmt.Sprintf("dylaris:server:%s:switch_failed", uuid)).Val(); ttl <= 0 {
				t.Errorf("report TTL = %v, want one", ttl)
			}
			if got := readActiveSubServer(dir); got != "a" {
				t.Errorf(".active_server = %q, want a", got)
			}
		})
	}
}
