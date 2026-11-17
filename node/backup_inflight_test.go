package main

import (
	"sync"
	"testing"
)

// TestInflightSet covers the property the backup and restore workers rely on:
// the command queue is at-least-once (pkg/queue reclaims stale pending entries
// and reprocesses its own pending list on reconnect), so the same restore CAN
// arrive twice. Observed live after a Core restart mid-restore - two
// extractions into one directory, each stopping and restarting the container
// under the other.
func TestInflightSet(t *testing.T) {
	s := newInflightSet()

	if !s.enter("1") {
		t.Fatal("first enter was refused")
	}
	if s.enter("1") {
		t.Error("a redelivery of an in-flight id was allowed to start")
	}
	// A different job must not be blocked by an unrelated one.
	if !s.enter("2") {
		t.Error("an unrelated id was blocked")
	}

	// After the work finishes the id must be usable again: a LATER, genuine
	// re-run of the same restore has to be able to proceed.
	s.leave("1")
	if !s.enter("1") {
		t.Error("id could not be re-entered after leaving; a legitimate re-run would be refused forever")
	}
}

// TestInflightSetIsConcurrencySafe: the two deliveries land on separate
// goroutines, so exactly one winner is the whole point. Run with -race.
func TestInflightSetIsConcurrencySafe(t *testing.T) {
	s := newInflightSet()
	const n = 50

	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.enter("same") {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if won != 1 {
		t.Errorf("%d goroutines entered, want exactly 1", won)
	}
}

// The two sets must be independent: a restore and a backup carrying the same
// numeric id are different work.
func TestBackupAndRestoreSetsAreIndependent(t *testing.T) {
	a, b := newInflightSet(), newInflightSet()
	if !a.enter("5") {
		t.Fatal("enter a")
	}
	if !b.enter("5") {
		t.Error("the same id in the other set was blocked; a backup and a restore would serialise against each other")
	}
}

// A backup archiving a server while a reinstall, version update, storage move
// or copy rewrote its tree reported success for an archive missing the parts
// that were moved aside, and a restore ran beside them the same way. Each side
// must refuse while the other runs, and leave must lift it.
func TestTreeChangesExcludeBackupAndRestoreOfTheServer(t *testing.T) {
	const uuid = "tree-change-interlock"
	cases := []struct {
		name  string
		enter func() (leave func(), reason string)
	}{
		{"backup", func() (func(), string) {
			return func() { backupsInFlight.leave("server:" + uuid) }, enterBackupOfServer(uuid)
		}},
		{"restore", func() (func(), string) {
			return func() { restoreServersInFlight.leave(uuid) }, enterRestoreOfServer(uuid)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" first", func(t *testing.T) {
			leave, reason := tc.enter()
			if reason != "" {
				t.Fatalf("%s refused on an idle server: %s", tc.name, reason)
			}
			if _, reason := enterTreeChangeOfServer(uuid); reason == "" {
				t.Errorf("a tree change started while a %s of the server ran", tc.name)
			}
			leave()
			leaveTree, reason := enterTreeChangeOfServer(uuid)
			if reason != "" {
				t.Fatalf("tree change still refused after the %s left: %s", tc.name, reason)
			}
			leaveTree()
		})
		t.Run("tree change first, "+tc.name+" second", func(t *testing.T) {
			// Two at once (a reinstall beside a storage move): the second's
			// leave must not lift the first's hold.
			leaveA, _ := enterTreeChangeOfServer(uuid)
			leaveB, _ := enterTreeChangeOfServer(uuid)
			leaveB()
			if leave, reason := tc.enter(); reason == "" {
				leave()
				t.Errorf("a %s started while a tree change of the server ran", tc.name)
			}
			leaveA()
			leave, reason := tc.enter()
			if reason != "" {
				t.Fatalf("%s still refused after the tree changes left: %s", tc.name, reason)
			}
			leave()
		})
	}
}
