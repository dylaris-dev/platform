package main

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
)

func TestMemGuardStep(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	type sample struct {
		at           time.Duration
		used         int64
		warn, critic bool
	}
	cases := []struct {
		name    string
		limit   int64
		samples []sample
	}{
		{"below warn does nothing (prod healthy server sits ~90.5%)", 1000, []sample{{0, 905, false, false}, {time.Hour, 949, false, false}}},
		{"warn at 95 percent", 1000, []sample{{0, 950, true, false}}},
		{"warn rate limited to once per 5 min", 1000, []sample{
			{0, 960, true, false},
			{4 * time.Minute, 960, false, false},
			{5 * time.Minute, 960, true, false},
		}},
		{"critical needs 30s held at 98", 1000, []sample{
			{0, 980, true, false},
			{29 * time.Second, 990, false, false},
			{30 * time.Second, 985, false, true},
		}},
		{"97 is not critical", 1000, []sample{{0, 979, true, false}, {time.Minute, 979, false, false}}},
		{"a dip below 98 restarts the hold", 1000, []sample{
			{0, 980, true, false},
			{20 * time.Second, 970, false, false},
			{40 * time.Second, 980, false, false},
			{69 * time.Second, 980, false, false},
			{70 * time.Second, 980, false, true},
		}},
		{"critical fires once until below 93", 1000, []sample{
			{0, 990, true, false},
			{30 * time.Second, 990, false, true},
			{2 * time.Minute, 990, false, false},
			{3 * time.Minute, 940, false, false}, // above rearm: still cooled down
			{4 * time.Minute, 990, false, false},
			{5 * time.Minute, 920, false, false}, // rearmed
			{6 * time.Minute, 990, true, false},
			{6*time.Minute + 30*time.Second, 990, false, true},
		}},
		{"no limit means no guard", 0, []sample{{0, 5000, false, false}, {time.Minute, 5000, false, false}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var g memGuard
			for i, s := range tc.samples {
				w, c := g.step(t0.Add(s.at), s.used, tc.limit)
				if w != s.warn || c != s.critic {
					t.Fatalf("sample %d (%s, %d/%d): got warn=%v critical=%v, want %v %v", i, s.at, s.used, tc.limit, w, c, s.warn, s.critic)
				}
			}
		})
	}
}

func TestOOMEventServerUUID(t *testing.T) {
	const uuid = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
	msg := func(action events.Action, attrs map[string]string) events.Message {
		return events.Message{Type: events.ContainerEventType, Action: action, Actor: events.Actor{Attributes: attrs}}
	}
	cases := []struct {
		name string
		msg  events.Message
		self []string
		want string
	}{
		{"oom on our mc container", msg(events.ActionOOM, map[string]string{"name": "mc_" + uuid, ownerLabel: "node-1"}), []string{"node-1"}, uuid},
		{"unlabelled container is ours", msg(events.ActionOOM, map[string]string{"name": "mc_" + uuid}), []string{"node-1"}, uuid},
		{"another node's container", msg(events.ActionOOM, map[string]string{"name": "mc_" + uuid, ownerLabel: "node-2"}), []string{"node-1"}, ""},
		{"not an mc container", msg(events.ActionOOM, map[string]string{"name": "dylaris-node"}), nil, ""},
		{"die is not oom", msg(events.ActionDie, map[string]string{"name": "mc_" + uuid, "exitCode": "137"}), nil, ""},
		{"bad uuid", msg(events.ActionOOM, map[string]string{"name": "mc_../x"}), nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := oomEventServerUUID(tc.msg, tc.self)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("got %q %v, want %q", got, ok, tc.want)
			}
		})
	}
}

func TestGuardMemoryBytes(t *testing.T) {
	cases := []struct {
		name     string
		st       map[string]uint64
		fallback uint64
		want     uint64
	}{
		{"cgroup v2: anon+shmem+kernel, file cache ignored", map[string]uint64{"anon": 3250, "shmem": 5, "kernel": 11, "file": 30, "active_file": 20, "inactive_file": 10}, 3300, 3266},
		{"cgroup v2: reclaimable slab is not counted", map[string]uint64{"anon": 3250, "shmem": 5, "kernel": 11, "slab_reclaimable": 4, "file": 30}, 3300, 3262},
		{"cgroup v2: slab_reclaimable without kernel cannot underflow", map[string]uint64{"anon": 100, "slab_reclaimable": 7}, 200, 100},
		{"cgroup v2 without kernel key", map[string]uint64{"anon": 100, "shmem": 2, "file": 900}, 1002, 102},
		{"cgroup v1 hierarchical", map[string]uint64{"total_rss": 200, "total_shmem": 3, "rss": 150, "shmem": 1, "total_cache": 800}, 1003, 203},
		{"cgroup v1 flat", map[string]uint64{"rss": 150, "shmem": 1, "cache": 800}, 951, 151},
		{"neither: the displayed figure", map[string]uint64{"cache": 800}, 951, 951},
		{"no map at all", nil, 42, 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := guardMemoryBytes(tc.st, tc.fallback); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBackupRunningFollowsTheBackupGuard(t *testing.T) {
	const uuid = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
	if backupRunning(uuid) {
		t.Fatal("reported a backup before one started")
	}
	// The key RunBackup enters for the whole archive.
	if !backupsInFlight.enter("server:" + uuid) {
		t.Fatal("could not enter the backup guard")
	}
	if !backupRunning(uuid) {
		t.Error("a running backup was not seen, save-all would hit the archive")
	}
	if backupRunning("11111111-2222-3333-4444-555555555555") {
		t.Error("another server's backup was reported")
	}
	backupsInFlight.leave("server:" + uuid)
	if backupRunning(uuid) {
		t.Error("the backup was still reported after it ended")
	}
}
