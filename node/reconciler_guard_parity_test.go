package main

import (
	"os"
	"strings"
	"testing"
)

// The reconciler has two passes that can put a server back into service: the
// crash-restart pass in StartReconciler and the recreate-from-disk pass in
// reconcileDeletedContainers. They need the SAME three holds, because all three
// exist for the same reason - the status key they would otherwise read is a
// mailbox Core drains every 5 seconds, so protectedStatuses cannot be trusted to
// still hold the answer. isDiskFull was present in one pass and missing in the
// other, which would have let a recreate walk straight past the disk guard.
//
// This reads source text, so it proves the CALL is there and nothing about the
// order or the branch it sits in. That is exactly the regression worth pinning:
// the guard was not weakened, it was absent.
func TestBothReconcilerPassesTakeTheSameHolds(t *testing.T) {
	src, err := os.ReadFile("reconciler.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	body := string(src)

	// Both passes take their holds from ONE function now, which also holds a
	// server this node staged a move of; the pass that recreated without it
	// would have been the next gap of the same kind.
	deleted := between(t, body, "func reconcileDeletedContainers(", "func restartBlocked(")
	crashed := after(t, body, "func StartReconciler(")
	holds := between(t, body, "func restartBlocked(", "func StartReconciler(")

	for name, pass := range map[string]string{"reconcileDeletedContainers": deleted, "StartReconciler's crash-restart pass": crashed} {
		if !strings.Contains(pass, "restartBlocked(") {
			t.Errorf("%s does not consult restartBlocked", name)
		}
	}
	for _, guard := range []string{"protectedStatuses[", "isNodeBusy(", "isDiskFull(", "migrationInFlight("} {
		if !strings.Contains(holds, guard) {
			t.Errorf("restartBlocked does not consult %s", guard)
		}
	}
}

func between(t *testing.T, s, from, to string) string {
	t.Helper()
	i := strings.Index(s, from)
	j := strings.Index(s, to)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("cannot slice source between %q and %q", from, to)
	}
	return s[i:j]
}

func after(t *testing.T, s, from string) string {
	t.Helper()
	i := strings.Index(s, from)
	if i < 0 {
		t.Fatalf("cannot find %q in source", from)
	}
	return s[i:]
}
