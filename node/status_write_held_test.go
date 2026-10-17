package main

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The two places the collector writes a status disagreed. Both read the status
// key, but Core's watcher drains that key every 5 seconds, so a hold posted
// there is invisible a moment later. Measured from Core's own log:
//
//	19:51:43  stopping  -> disk_full
//	19:51:48  disk_full -> online
//
// leaving a stopped server showing as online and silencing the power route's
// storage-limit gate, which keys on that status.
func TestStatusWriteHeld(t *testing.T) {
	const uuid = "22222222-2222-4222-8222-222222222222"

	tests := []struct {
		name          string
		currentStatus string
		markerSet     bool
		stopSet       bool
		want          bool
		why           string
	}{
		{
			name: "nothing held, mailbox empty",
			want: false,
			why:  "the ordinary case: the collector may report what it sees",
		},
		{
			name:          "a hold still sitting in the mailbox",
			currentStatus: "disk_full",
			markerSet:     true,
			want:          true,
			why:           "this is the only window the old check could see",
		},
		{
			name:      "the mailbox already drained, marker still set",
			markerSet: true,
			want:      true,
			why:       "THIS is the bug: five seconds after the hold was posted, the key is empty and the old check passed",
		},
		{
			name:          "installing, mailbox not yet drained",
			currentStatus: "installing",
			want:          true,
			why:           "an in-flight install must not be relabelled",
		},
		{
			name:          "stopping, mailbox not yet drained",
			currentStatus: "stopping",
			want:          true,
			why:           "a deliberate shutdown must not be reported as online again",
		},
		{
			name:    "a stop in progress, mailbox already drained",
			stopSet: true,
			want:    true,
			why:     "the container still runs while it stops; reporting it online again made the panel flicker",
		},
		{
			name:          "a status nobody protects",
			currentStatus: "online",
			want:          false,
			why:           "no hold, so writing is fine",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr, err := miniredis.Run()
			if err != nil {
				t.Fatalf("miniredis: %v", err)
			}
			defer mr.Close()
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			if tt.markerSet {
				mr.Set(diskFullKey(uuid), "1")
			}
			if tt.stopSet {
				mr.Set(stopRequestedKey(uuid), "1")
			}
			if got := statusWriteHeld(context.Background(), rdb, uuid, tt.currentStatus); got != tt.want {
				t.Errorf("statusWriteHeld = %v, want %v: %s", got, tt.want, tt.why)
			}
		})
	}
}
