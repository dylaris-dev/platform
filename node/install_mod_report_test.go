package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"dylaris-pkg/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The panel shows an install as running until this report arrives, so every
// outcome has to send exactly one, carrying the node's reason on a failure.
func TestInstallModReportsEachOutcomeOnce(t *testing.T) {
	tests := []struct {
		name        string
		fail        error
		wantStatus  string
		wantMessage string
	}{
		{name: "success", wantStatus: "installed"},
		{name: "hash mismatch", fail: errors.New("sha512 mismatch: want a got b"), wantStatus: "failed",
			wantMessage: "download failed for spark-1.1.jar: sha512 mismatch: want a got b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubDownload(t, tt.fail)
			sm, uuid, _ := installFixture(t)
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { rdb.Close() })
			prev := nodeID
			nodeID = "node-report"
			t.Cleanup(func() { nodeID = prev })

			sub := rdb.Subscribe(context.Background(), queue.ModResultsChannel(nodeID))
			defer sub.Close()
			if _, err := sub.Receive(context.Background()); err != nil {
				t.Fatal(err)
			}

			runInstallMod(context.Background(), rdb, sm, replacePayload(uuid, ""))

			var got []modInstallResult
			deadline := time.After(200 * time.Millisecond)
		collect:
			for {
				select {
				case msg := <-sub.Channel():
					var r modInstallResult
					if err := json.Unmarshal([]byte(msg.Payload), &r); err != nil {
						t.Fatal(err)
					}
					got = append(got, r)
				case <-deadline:
					break collect
				}
			}
			if len(got) != 1 {
				t.Fatalf("reports = %d, want 1 (%+v)", len(got), got)
			}
			r := got[0]
			if r.Status != tt.wantStatus || r.Message != tt.wantMessage ||
				r.InstallID != "attempt-1" || r.ServerID != 7 || r.ProjectID != "spark" || r.SubServer != "survival" {
				t.Errorf("report = %+v, want status %q message %q", r, tt.wantStatus, tt.wantMessage)
			}
		})
	}
}
