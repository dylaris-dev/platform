package storageapi

import (
	"context"
	"fmt"
	"testing"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/services"
	"dylaris-core/services/storagereach"
	"dylaris-core/storage"
)

// The shared-storage proof lives in checkSharedStorageReachable and the
// settings save goes through it. The storage-migration config switch persists
// a target config through the SAME writer but used to skip the guard
// entirely, so a migration to a host-path target sailed straight past the
// check SaveConfig enforces and split file storage across every online Core -
// the exact state the guard exists to prevent. These pin that SwitchConfig now
// shares it.

// switchResolverFor builds a resolver over a fake store and a miniredis seeded
// with the named Cores. Gate and S3 are wired because a SUCCESSFUL switch calls
// persistCoreStorageConfig, which syncs them. StorageReach is wired with a
// LocalProvider factory rooted at a fresh temp dir per call, so a resolver
// built with more than one Core online can actually run a round rather than
// refusing outright because no verifier is present.
func switchResolverFor(t *testing.T, st *multiCoreFakeStore, coreIDs ...string) *StorageDataSetResolver {
	t.Helper()
	rdb := multiCoreRedis(t, coreIDs...)
	return &StorageDataSetResolver{state: &handlers.AppState{
		Store:       st,
		Redis:       rdb,
		StorageGate: storage.NewGate(),
		StorageS3:   storage.NewS3Resilience(),
		StorageReach: storagereach.NewService(storagereach.ServiceDeps{
			Redis: rdb, CoreID: "core-a", NewProvider: sharedLocalFactory(t.TempDir()),
		}),
	}}
}

func hostPathTarget() services.StorageTargetConfig {
	return services.StorageTargetConfig{Backend: "path", Path: "/mnt/shared", PathConfirmed: true}
}

func s3Target() services.StorageTargetConfig {
	return services.StorageTargetConfig{Backend: "s3", S3Bucket: "b", S3AccessKey: "AKIA", S3SecretKey: "secret"}
}

// TestSwitchConfig_RefusesHostPathOnMultipleCores is the load-bearing one: the
// bypass this closes. core-b heartbeats but never runs an actual participant,
// so the round times out with core-b no-response - fail-closed, same as an
// unproven SaveConfig.
func TestSwitchConfig_RefusesHostPathOnMultipleCores(t *testing.T) {
	st := multiCoreState(t, map[string]string{})
	r := switchResolverFor(t, st, "core-a", "core-b")
	r.state.SetReachRoundDeadline(150 * time.Millisecond)

	err := r.SwitchConfig(context.Background(), CoreStorageDataSetID, hostPathTarget())

	if err == nil {
		t.Fatal("SwitchConfig to a host path with two Cores online returned nil, want a refusal")
	}
	if st.writes != 0 {
		t.Errorf("a refused switch persisted %d settings; it must persist none", st.writes)
	}
}

// TestSwitchConfig_AllowsHostPathOnASingleCore is the negative control: the
// guard must not block the switch a single-Core deployment is entitled to.
func TestSwitchConfig_AllowsHostPathOnASingleCore(t *testing.T) {
	st := multiCoreState(t, map[string]string{})
	r := switchResolverFor(t, st, "core-a")

	if err := r.SwitchConfig(context.Background(), CoreStorageDataSetID, hostPathTarget()); err != nil {
		t.Fatalf("SwitchConfig to a host path with one Core online = %v, want nil", err)
	}
	if st.values[handlers.KeyCoreStorageBackend] != "path" {
		t.Errorf("backend persisted as %q, want %q", st.values[handlers.KeyCoreStorageBackend], "path")
	}
}

// TestSwitchConfig_AllowsS3OnMultipleCores is the positive control, updated
// for the round-based guard: s3 is no longer a blanket bypass on multi-Core
// deployments (the old count-only guard's stance) - it must pass the same
// proof a host path does. core-b runs a real participant goroutine so the
// round can actually confirm both Cores and the switch succeeds.
func TestSwitchConfig_AllowsS3OnMultipleCores(t *testing.T) {
	st := multiCoreState(t, map[string]string{})
	rdb := multiCoreRedis(t, "core-a", "core-b")
	factory := sharedLocalFactory(t.TempDir())
	r := &StorageDataSetResolver{state: &handlers.AppState{
		Store:       st,
		Redis:       rdb,
		StorageGate: storage.NewGate(),
		StorageS3:   storage.NewS3Resilience(),
		StorageReach: storagereach.NewService(storagereach.ServiceDeps{
			Redis: rdb, CoreID: "core-a", NewProvider: factory,
		}),
	}}

	done := make(chan error, 1)
	go func() {
		ctx := context.Background()
		// 3000 x 10ms = 30s. This bounds how long the participant waits to be
		// SCHEDULED and notice the round, which on a runner executing the whole
		// job matrix at once is not the same as how long the work takes. It only
		// runs out on the failing path - a passing round ends as soon as the
		// reports are in - so a generous bound costs nothing and removes a
		// scheduling race that has now failed CI twice.
		for i := 0; i < 3000; i++ {
			id, err := storagereach.PendingRoundID(ctx, rdb)
			if err == nil && id != "" {
				done <- storagereach.RunParticipant(ctx, rdb, "core-b", id, factory)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		done <- fmt.Errorf("core-b never saw a round")
	}()

	if err := r.SwitchConfig(context.Background(), CoreStorageDataSetID, s3Target()); err != nil {
		t.Fatalf("SwitchConfig to s3 with two Cores online = %v, want nil", err)
	}
	if perr := <-done; perr != nil {
		t.Fatalf("core-b participant: %v", perr)
	}
	if st.values[handlers.KeyCoreStorageBackend] != "s3" {
		t.Errorf("backend persisted as %q, want %q", st.values[handlers.KeyCoreStorageBackend], "s3")
	}
}

// TestSwitchConfig_RefusesHostPathWhenTheCountCannotBeTaken mirrors the save
// guard's fail-closed stance: an unverifiable count refuses rather than
// persists, because letting it through is the split-storage outcome the whole
// check exists to stop.
func TestSwitchConfig_RefusesHostPathWhenTheCountCannotBeTaken(t *testing.T) {
	st := multiCoreState(t, map[string]string{})
	r := &StorageDataSetResolver{state: &handlers.AppState{
		Store:       st,
		Redis:       downRedis(t),
		StorageGate: storage.NewGate(),
		StorageS3:   storage.NewS3Resilience(),
	}}

	if err := r.SwitchConfig(context.Background(), CoreStorageDataSetID, hostPathTarget()); err == nil {
		t.Fatal("SwitchConfig to a host path with an unreachable Redis returned nil, want a refusal")
	}
	if st.writes != 0 {
		t.Errorf("a refused switch persisted %d settings; it must persist none", st.writes)
	}
}
