package health

import (
	"context"
	"encoding/json"
	"testing"

	"dylaris-core/services"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func coresRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

func seedCore(t *testing.T, mr *miniredis.Miniredis, id, version string) {
	t.Helper()
	b, err := json.Marshal(services.CoreHeartbeat{ID: id, Version: version})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mr.Set("dylaris:core:"+id, string(b))
}

func coresHandler(rdb *redis.Client, selfID, selfVersion string) *UpdatesHandler {
	return &UpdatesHandler{d: Deps{Redis: rdb, CoreID: selfID, ReleaseVersion: selfVersion}}
}

func labels(insts []instance) map[string]string {
	out := map[string]string{}
	for _, i := range insts {
		out[i.Label] = i.Version
	}
	return out
}

// The defect this replaced: two Cores were reported as one, because the handler
// only ever described the process answering the request.
func TestCoresReportsEveryInstance(t *testing.T) {
	rdb, mr := coresRedis(t)
	seedCore(t, mr, "core-a", "2026.08.20")
	seedCore(t, mr, "core-b", "2026.08.28")

	got := labels(coresHandler(rdb, "core-a", "2026.08.20").cores(context.Background(), nil))

	want := map[string]string{"core-a": "2026.08.20", "core-b": "2026.08.28"}
	if len(got) != len(want) {
		t.Fatalf("got %d instances (%v), want %d", len(got), got, len(want))
	}
	for id, v := range want {
		if got[id] != v {
			t.Errorf("%s reported %q, want %q", id, got[id], v)
		}
	}
}

// The heartbeat has a 30s TTL, so right after a restart onto a new build the
// stored copy still names the old one. The running process knows better about
// itself and must win.
func TestCoresPrefersItsOwnVersionOverItsHeartbeat(t *testing.T) {
	rdb, mr := coresRedis(t)
	seedCore(t, mr, "core-a", "2026.08.20")

	got := labels(coresHandler(rdb, "core-a", "2026.08.28").cores(context.Background(), nil))

	if got["core-a"] != "2026.08.28" {
		t.Errorf("core-a reported %q, want the running build 2026.08.28", got["core-a"])
	}
}

// A Core whose own heartbeat has not landed yet must still appear: the answer
// would otherwise omit the very instance the operator is looking at.
func TestCoresAlwaysIncludesItself(t *testing.T) {
	rdb, mr := coresRedis(t)
	seedCore(t, mr, "core-b", "2026.08.28")

	got := labels(coresHandler(rdb, "core-a", "2026.08.28").cores(context.Background(), nil))

	if _, ok := got["core-a"]; !ok {
		t.Errorf("the answering Core is missing from %v", got)
	}
	if len(got) != 2 {
		t.Errorf("got %v, want both Cores", got)
	}
}

// Redis being unreachable is not a reason to answer nothing: the version this
// process was built from is known for certain either way.
func TestCoresFallsBackToItselfWithoutRedis(t *testing.T) {
	rdb, mr := coresRedis(t)
	mr.Close()

	got := labels(coresHandler(rdb, "core-a", "2026.08.28").cores(context.Background(), nil))

	if len(got) != 1 || got["core-a"] != "2026.08.28" {
		t.Errorf("got %v, want just this Core at its own version", got)
	}
}

// An unstamped Core reports no version rather than an empty one that would
// order below every release and flag itself as behind.
func TestCoresUnstampedReportsNoVersion(t *testing.T) {
	rdb, _ := coresRedis(t)

	insts := coresHandler(rdb, "core-a", "").cores(context.Background(), nil)

	if len(insts) != 1 {
		t.Fatalf("got %d instances, want 1", len(insts))
	}
	if insts[0].Version != "" || insts[0].Outdated {
		t.Errorf("unstamped Core reported version=%q outdated=%v, want empty and not outdated",
			insts[0].Version, insts[0].Outdated)
	}
}
