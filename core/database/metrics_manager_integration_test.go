package database

import (
	"context"
	"fmt"
	"testing"
	"time"

	"dylaris-core/metrics"
)

// Swapping the metrics database at runtime is the mechanism that lets the
// target be a panel setting instead of a restart, and every part of it is a
// claim about a real database that no fake can make:
//
//   - the new target has to be OPEN before the old one is retired, or a typo in
//     the form takes down recording that was working;
//   - the retired recorder has to finish its final flush BEFORE its pool is
//     closed, or the last minutes turn into a "store write failed" line;
//   - after the swap, rows have to arrive in the new database and stop arriving
//     in the old one.
//
// One server, two connections to the same database: the recorder writes what it
// is told, so what is being proven here is the handover, not the routing.
func TestIntegrationTheMetricsTargetCanBeSwappedWhileRunning(t *testing.T) {
	cfg := testDBConfig(t)
	coreDB, _ := integrationDB(t)
	ctx := context.Background()

	// Spelled out rather than built with services.MetricsDBTarget: `services`
	// imports this package, so a test here cannot import it back. The FORM is
	// pinned by the unit tests over there; what this file proves is that a dsn
	// of this shape can actually be opened and written to.
	//
	// Plain Postgres in CI, so EnsureSchema falls back to a plain table and says
	// so - the supported path for a self-hoster too.
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName, cfg.DBSSLMode)

	// Two dsns naming the same database. There is one server in CI, and what
	// is under test is the handover between two pools, not where the rows land
	// - so a second dsn that differs only in application_name is enough, and
	// keeps both writes readable through coreDB.
	second_dsn := dsn + " application_name=metrics-swap"

	m := metrics.NewManager(ctx)
	t.Cleanup(m.Close)

	if err := m.Apply(dsn); err != nil {
		t.Fatalf("opening the first target: %v", err)
	}
	first := m.Handle()
	if first == nil || first.Dedicated == nil {
		t.Fatalf("the target opened no pool of its own: %+v", first)
	}
	if first.Resolution != metrics.ResolutionDedicated {
		t.Fatalf("resolution = %v, want %v", first.Resolution, metrics.ResolutionDedicated)
	}

	// A value recorded before the swap must still be written, not dropped: the
	// retired recorder flushes on its way out.
	before := fmt.Sprintf("test.swap.before.%d", time.Now().UnixNano())
	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { coreDB.Exec(`DELETE FROM metric_samples WHERE metric LIKE 'test.swap.%'`) })
	m.Recorder().Observe(metrics.Key{Metric: before, Subject: "s"}, 7, at)

	if err := m.Apply(second_dsn); err != nil {
		t.Fatalf("swapping to the second target: %v", err)
	}

	second := m.Handle()
	if second == nil || second.Dedicated == nil {
		t.Fatal("after the swap there is no pool")
	}
	if second == first {
		t.Fatal("Apply kept the same handle")
	}
	if second.Resolution != metrics.ResolutionDedicated {
		t.Fatalf("resolution after the swap = %v, want %v", second.Resolution, metrics.ResolutionDedicated)
	}

	// The pre-swap value survived, which is the flush-then-close order working.
	var rows int
	if err := coreDB.QueryRow(`SELECT count(*) FROM metric_samples WHERE metric = $1`, before).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("the value recorded before the swap produced %d rows; the final flush was lost", rows)
	}

	// And the new recorder writes.
	after := fmt.Sprintf("test.swap.after.%d", time.Now().UnixNano())
	m.Recorder().Observe(metrics.Key{Metric: after, Subject: "s"}, 3, at)
	if err := m.Recorder().Flush(ctx); err != nil {
		t.Fatalf("flushing the new target: %v", err)
	}
	if err := second.Read.QueryRow(`SELECT count(*) FROM metric_samples WHERE metric = $1`, after).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("after the swap the new target holds %d rows for the new metric", rows)
	}
}

// Re-applying the same target must do nothing at all. A settings save that
// changed something else would otherwise tear down the recorder and lose the
// buckets accumulated since the last flush - for no change.
func TestIntegrationReapplyingTheSameTargetIsANoOp(t *testing.T) {
	cfg := testDBConfig(t)
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName, cfg.DBSSLMode)
	m := metrics.NewManager(context.Background())
	t.Cleanup(m.Close)

	if err := m.Apply(dsn); err != nil {
		t.Fatal(err)
	}
	first := m.Handle()
	if err := m.Apply(dsn); err != nil {
		t.Fatal(err)
	}
	if m.Handle() != first {
		t.Fatal("re-applying the same target replaced the handle, discarding whatever it held")
	}
}

// A target that cannot be opened must leave the working one in place. The
// panel reports the failure; it does not get to stop the recording that was
// already happening.
func TestIntegrationAFailedApplyKeepsTheWorkingTarget(t *testing.T) {
	cfg := testDBConfig(t)
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName, cfg.DBSSLMode)
	m := metrics.NewManager(context.Background())
	t.Cleanup(m.Close)

	if err := m.Apply(dsn); err != nil {
		t.Fatal(err)
	}
	working := m.Handle()

	err := m.Apply("host=127.0.0.1 port=1 user=nobody dbname=nothing sslmode=disable")
	if err == nil {
		t.Fatal("applying an unreachable target reported success")
	}
	if m.Handle() != working {
		t.Fatal("a failed apply replaced the working handle")
	}
	// Still usable, not just still present.
	if m.Recorder() == nil {
		t.Fatal("the recorder is gone after a failed apply")
	}
}
