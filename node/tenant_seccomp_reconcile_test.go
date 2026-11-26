package main

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestOnTenantSeccomp(t *testing.T) {
	current := "seccomp=" + tenantSeccompProfile
	for _, tc := range []struct {
		name string
		opts []string
		want bool
	}{
		{"created before any hardening", nil, false},
		{"no-new-privileges only (before 92d1c4b1)", []string{"no-new-privileges:true"}, false},
		{"another profile", []string{"no-new-privileges:true", `seccomp={"defaultAction":"SCMP_ACT_ALLOW"}`}, false},
		{"unconfined", []string{"seccomp=unconfined"}, false},
		{"the profile with a byte changed", []string{current + " "}, false},
		{"current", []string{"no-new-privileges:true", current}, true},
		{"current, other options around it", []string{"label=disable", current, "no-new-privileges:true"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := onTenantSeccomp(tc.opts); got != tc.want {
				t.Errorf("onTenantSeccomp = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every reason a panel restart or the reconciler would leave a server alone is
// a reason this pass leaves it on its old profile.
func TestSeccompRecreateHold(t *testing.T) {
	const uuid = sharedDaemonUUID
	key := func(s string) string { return "dylaris:server:" + uuid + ":" + s }
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, set func(k, v string))
		want  string // substring of the hold; "" = recreate
	}{
		{"nothing set", func(*testing.T, func(k, v string)) {}, ""},
		{"desired online", func(_ *testing.T, set func(k, v string)) { set(desiredStateKey(uuid), "online") }, ""},
		{"desired stopped", func(_ *testing.T, set func(k, v string)) { set(desiredStateKey(uuid), "stopped") }, "meant to be stopped"},
		{"installing", func(_ *testing.T, set func(k, v string)) { set(key("status"), "installing") }, "held"},
		{"suspended", func(_ *testing.T, set func(k, v string)) { set(key("status"), "suspended") }, "held"},
		{"node_busy", func(_ *testing.T, set func(k, v string)) { set(nodeBusyKey(uuid), "1") }, "held"},
		{"disk full", func(_ *testing.T, set func(k, v string)) { set(diskFullKey(uuid), "1") }, "held"},
		{"backup", func(t *testing.T, _ func(k, v string)) { inflight(t, backupsInFlight, "server:"+uuid) }, "backup"},
		{"restore", func(t *testing.T, _ func(k, v string)) { inflight(t, restoreServersInFlight, uuid) }, "restore"},
		{"setup", func(t *testing.T, _ func(k, v string)) { inflight(t, commandsInFlight, "setup:"+uuid) }, "setup"},
		{"reinstall", func(t *testing.T, _ func(k, v string)) { inflight(t, commandsInFlight, "reinstall:"+uuid) }, "reinstall"},
		{"storage move", func(t *testing.T, _ func(k, v string)) { inflight(t, commandsInFlight, "migrate_storage:"+uuid) }, "migrate_storage"},
		{"software change", func(t *testing.T, _ func(k, v string)) { inflight(t, commandsInFlight, softwareKey(uuid)) }, "software"},
		{"another server's backup", func(t *testing.T, _ func(k, v string)) { inflight(t, backupsInFlight, "server:other") }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newMiniRedis(t)
			tc.setup(t, func(k, v string) { rdb.Set(context.Background(), k, v, 0) })
			storage := NewStorageManager(t.TempDir(), rdb)
			got := seccompRecreateHold(context.Background(), rdb, storage, uuid)
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("hold = %q, want one naming %q", got, tc.want)
			}
		})
	}
}

func inflight(t *testing.T, s *inflightSet, id string) {
	t.Helper()
	if !s.enter(id) {
		t.Fatalf("%s already in flight", id)
	}
	t.Cleanup(func() { s.leave(id) })
}

// A container on the profile is never touched, however often the pass runs; a
// stale one that is stopped or held is skipped without a Docker change.
func TestReconcileSeccompOneTouchesOnlyStaleRunningServers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		running  bool
		opts     []string // nil = current profile
		busy     bool
		outcome  string
		noChange bool
	}{
		{"running, current", true, nil, false, "", true},
		{"stopped, current", false, nil, false, "", true},
		{"stopped, stale", false, []string{"no-new-privileges:true"}, false, "no longer running", true},
		{"running, stale, node_busy", true, []string{"no-new-privileges:true"}, true, "held", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dm, d := newSharedDaemon(t, "node-a", tc.running, nil)
			d.securityOpt = tc.opts
			rdb := newMiniRedis(t)
			dm.storageMgr = NewStorageManager(t.TempDir(), rdb)
			if tc.busy {
				rdb.Set(context.Background(), nodeBusyKey(sharedDaemonUUID), "1", 0)
			}
			for range 2 {
				got, err := dm.reconcileSeccompOne(rdb, sharedDaemonUUID)
				if err != nil {
					t.Fatal(err)
				}
				if (tc.outcome == "") != (got == "") || !strings.Contains(got, tc.outcome) {
					t.Errorf("outcome = %q, want one naming %q", got, tc.outcome)
				}
			}
			if ch := d.changed(); tc.noChange && len(ch) != 0 {
				t.Errorf("Docker changes %v, want none", ch)
			}
		})
	}
}

// A stopped container is left to its next start: every start of one created
// before the profile is a recreate, the reconciler's plain start included.
func TestStartRecreatesAContainerWithoutTheProfile(t *testing.T) {
	t.Cleanup(func() { daemonNoSeccomp.Store(false) })
	for _, tc := range []struct {
		name      string
		opts      []string
		noSeccomp bool
		recreate  bool
	}{
		{"before the profile", []string{"no-new-privileges:true"}, false, true},
		{"current", nil, false, false},
		{"daemon without seccomp", []string{"no-new-privileges:true"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemonNoSeccomp.Store(tc.noSeccomp)
			dm, d := newSharedDaemon(t, "node-a", false, nil)
			d.securityOpt = tc.opts
			dm.PowerAction(sharedDaemonUUID, "start")
			if got := slices.Contains(d.changed(), "create"); got != tc.recreate {
				t.Errorf("recreated = %v (%v), want %v", got, d.changed(), tc.recreate)
			}
		})
	}
}
