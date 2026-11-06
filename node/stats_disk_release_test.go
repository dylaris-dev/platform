package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/api/types/container"
	"github.com/redis/go-redis/v9"
)

// J49: the disk guard's own stop exits 0 like "stop" in the console, so once
// the hold was lifted the reconciler read it as the server stopping itself
// and kept it down for good. The stop the guard made must not count as one.
func TestTheDiskGuardsStopIsNotTheServerStoppingItself(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	const uuid = "dddddddd-1111-2222-3333-555555555555"
	const other = "dddddddd-1111-2222-3333-666666666666"
	t.Cleanup(func() { diskGuardTrips.Delete(uuid) })

	before := time.Now().Add(-time.Minute)
	holdForDiskFull(context.Background(), rdb, uuid, "live", &DiskUsagePayload{Total: 2, Limit: 1}, func() {})
	after := time.Now().Add(time.Minute)

	stoppedByGuard := &container.State{ExitCode: 0, StartedAt: before.Format(time.RFC3339Nano), FinishedAt: after.Format(time.RFC3339Nano)}
	laterRun := &container.State{ExitCode: 0, StartedAt: after.Format(time.RFC3339Nano), FinishedAt: after.Add(time.Minute).Format(time.RFC3339Nano)}
	stoppedBefore := &container.State{ExitCode: 0, StartedAt: before.Add(-time.Minute).Format(time.RFC3339Nano), FinishedAt: before.Format(time.RFC3339Nano)}
	for _, c := range []struct {
		name string
		uuid string
		st   *container.State
		want bool
	}{
		{"the run the guard stopped", uuid, stoppedByGuard, true},
		{"a later run stopped in the console", uuid, laterRun, false},
		{"a run that ended before the trip", uuid, stoppedBefore, false},
		{"a server the guard never tripped", other, stoppedByGuard, false},
		{"no state", uuid, nil, false},
	} {
		if got := stoppedByDiskGuard(c.uuid, c.st); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}

	// A hold lifted from a still-running container: the guard's stop did not
	// take, so a later clean stop of that run is the owner's own.
	releaseDiskHold(context.Background(), rdb, uuid, "live", &DiskUsagePayload{Total: 0, Limit: 1}, true)
	if stoppedByDiskGuard(uuid, stoppedByGuard) {
		t.Error("a run still running at the release still counts as the guard's stop")
	}
}
