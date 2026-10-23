package main

import (
	"context"
	"os"
	"regexp"
	"testing"
)

// Every node command that stops a container whose desired_state is still
// "online" and then rewrites its files MUST hold the busy interlock, or the
// reconciler starts the server back up in the middle of the work.
//
// backup_restore was the one that did not, and it is the most destructive of
// them: it renames the whole server directory away and swaps a staged one in.
// Caught live on the testbed - with a 646MB archive the unprotected window is
// only about two seconds, and the reconciler still landed in it:
//
//	19:00:29 Restore 6: stopping container mc_<uuid>
//	19:00:34 reconciler: restarting crashed container mc_<uuid> (attempt 1/3)
//	19:00:35 Restore 6: restarting container mc_<uuid>
//
// On remote backup storage the same window is minutes wide.
//
// This reads the source rather than driving the command, because RunRestore
// needs a DockerManager and real storage. What it pins is the thing that was
// missing: the interlock is taken on that path at all.
func TestDestructiveCommandsHoldTheBusyInterlock(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	for _, action := range []string{"setup", "reinstall", "migrate_storage", "backup_restore"} {
		t.Run(action, func(t *testing.T) {
			caseRe := regexp.MustCompile(`(?s)case "` + action + `":(.*?)\n\tcase "`)
			m := caseRe.FindStringSubmatch(text)
			if m == nil {
				t.Fatalf("case %q not found in the command switch", action)
			}
			if !regexp.MustCompile(`holdBusyStatus\(`).MatchString(m[1]) {
				t.Fatalf("case %q stops a container and rewrites its files but never takes the busy interlock", action)
			}
		})
	}
}

// The interlock is only worth anything if the reconciler actually consults it,
// so pin that too: isNodeBusy must report the key holdBusyStatus writes.
func TestBusyInterlockIsTheKeyTheReconcilerReads(t *testing.T) {
	rdb, _ := newBusyRedis(t)
	ctx := context.Background()

	if isNodeBusy(ctx, rdb, "srv-1") {
		t.Fatal("busy with nothing held")
	}
	release := holdBusyStatus(rdb, "srv-1", "restarting", busyStatusTTL)
	if !isNodeBusy(ctx, rdb, "srv-1") {
		t.Fatal("interlock held but isNodeBusy says otherwise")
	}
	release()
	if isNodeBusy(ctx, rdb, "srv-1") {
		t.Fatal("interlock still reported after release")
	}
}

// A stop inside a busy operation (restore, install, move) must not announce
// "stopping": the hold refreshes its own status every ttl/3, so the two writes
// took turns and the panel alternated restarting -> stopping -> restarting.
// A plain stop still announces it, before the wait (see gracefulStop).
func TestAStopInsideABusyHoldKeepsTheHoldsStatus(t *testing.T) {
	rdb, _ := newBusyRedis(t)
	ctx := context.Background()
	statusKey := "dylaris:server:srv-1:status"

	announceStopping(ctx, rdb, "srv-1")
	if got := rdb.Get(ctx, statusKey).Val(); got != "stopping" {
		t.Fatalf("a plain stop announced %q, want stopping", got)
	}

	release := holdBusyStatus(rdb, "srv-1", "restarting", busyStatusTTL)
	defer release()
	announceStopping(ctx, rdb, "srv-1")
	if got := rdb.Get(ctx, statusKey).Val(); got != "restarting" {
		t.Fatalf("a stop inside a restore announced %q, want the hold's restarting", got)
	}
}
