package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The quota path reuses the sub-server breakdown instead of walking every
// sub-server with du on each ten-second tick.
func TestSubServerSizesAreReused(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "survival"), 0o755); err != nil {
		t.Fatal(err)
	}
	first := subServerSizesFresh(dir)
	if err := os.WriteFile(filepath.Join(dir, "survival", "big"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if again := subServerSizesFresh(dir); again["survival"] != first["survival"] {
		t.Fatalf("the breakdown was walked again within the interval: %v then %v", first, again)
	}
}

// Pinned in the source: one disk measurement per server at a time.
func TestDiskMeasurementsDoNotStack(t *testing.T) {
	b, err := os.ReadFile("stats_collector.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "if !measuring.CompareAndSwap(false, true) {") || !strings.Contains(s, "defer measuring.Store(false)") {
		t.Error("a disk measurement can start while the previous one still runs")
	}
	q, err := os.ReadFile("disk_quota.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(q), "SubServers: subServerSizesFresh(") {
		t.Error("the quota path walks the sub-servers on every tick again")
	}
}
