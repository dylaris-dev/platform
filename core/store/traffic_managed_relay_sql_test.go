package store

import (
	"os"
	"strings"
	"testing"
)

// The managed relay subquery is spelled out in three statements so the
// prepare test sees each one whole. They must keep meaning the same cells as
// the constants the aggregator writes, or the pool and the grace snapshot
// would read different bytes.
func TestManagedRelaySQLMatchesTheConstants(t *testing.T) {
	src, err := os.ReadFile("traffic.go")
	if err != nil {
		t.Fatal(err)
	}
	pred := "AND r.kind = '" + TrafficKindRelay + "' AND r.product = '" + TrafficProductRoute + "')"
	if n := strings.Count(string(src), pred); n != 3 {
		t.Fatalf("%d statements carry %q, want 3 (GetTrafficUsage, StartTrafficGrace, EndTrafficGrace)", n, pred)
	}
}
