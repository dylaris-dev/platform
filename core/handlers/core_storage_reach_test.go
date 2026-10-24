package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"

	"dylaris-core/services"
	"dylaris-core/services/storagereach"
	"dylaris-core/storage"
)

// Redis seeding for these tests reuses multiCoreRedis and coreStorageFakeStore
// (core_storage_multicore_test.go / core_storage_builder_test.go) rather than
// declaring a second heartbeat-seeding helper for the same package.

func TestCoreStorageToReachConfig_PathBackend(t *testing.T) {
	got := CoreStorageToReachConfig(CoreStorageConfig{Backend: "path", Path: "/mnt/shared"})
	if got.Backend != "path" || got.Path != "/mnt/shared" {
		t.Fatalf("config = %+v, want the path backend carried over", got)
	}
}

func TestCoreStorageToReachConfig_S3CarriesTheCredential(t *testing.T) {
	// The participant builds a real client from this, so the secret has to
	// travel. If it stops travelling, every peer reports unreachable.
	got := CoreStorageToReachConfig(CoreStorageConfig{
		Backend: "s3", S3Endpoint: "https://s3.example", S3Bucket: "b",
		S3Region: "eu", S3AccessKey: "AK", S3SecretKey: "SK", S3Prefix: "p", S3PathStyle: true,
	})
	if got.S3Bucket != "b" || got.S3AccessKey != "AK" || got.S3SecretKey != "SK" {
		t.Fatalf("config = %+v, want the s3 identity and credential carried over", got)
	}
	if !got.S3PathStyle || got.S3Prefix != "p" || got.S3Region != "eu" || got.S3Endpoint != "https://s3.example" {
		t.Fatalf("config = %+v, want every s3 field carried over", got)
	}
}

func TestCheckSharedStorageReachable_SingleCoreSkipsTheRound(t *testing.T) {
	// One Core cannot fail a sharing check, and running a probe on every
	// single-instance save would be pure latency.
	rdb := multiCoreRedis(t, "core-a")
	s := &AppState{Redis: rdb, Store: &coreStorageFakeStore{values: map[string]string{}}}

	if err := s.checkSharedStorageReachable(context.Background(),
		CoreStorageConfig{Backend: "path", Path: "/mnt/shared"}); err != nil {
		t.Fatalf("checkSharedStorageReachable on a single Core = %v, want nil", err)
	}
}

// TestCheckSharedStorageReachable_ZeroOnlineCoresIsTreatedAsOne pins the other
// half of the "0 and 1 are both not-more-than-one" short-circuit: a fresh
// install whose own heartbeat has not landed in Redis yet must still be able
// to save its storage settings, not be refused because the online count reads
// as zero. This case existed in the old count-only guard's table and was
// dropped when that guard was replaced; the behavior itself was never
// removed (see the len(online) <= 1 check above), only its test coverage.
func TestCheckSharedStorageReachable_ZeroOnlineCoresIsTreatedAsOne(t *testing.T) {
	rdb := multiCoreRedis(t) // no heartbeats seeded: the online count is 0
	s := &AppState{Redis: rdb, Store: &coreStorageFakeStore{values: map[string]string{}}}

	if err := s.checkSharedStorageReachable(context.Background(),
		CoreStorageConfig{Backend: "path", Path: "/mnt/shared"}); err != nil {
		t.Fatalf("checkSharedStorageReachable with zero online Cores = %v, want nil (short-circuit)", err)
	}
}

func TestCheckSharedStorageReachable_RefusesWhenAPeerNeverReports(t *testing.T) {
	// Two Cores heartbeating, only one of them running: fail-closed. core-a
	// (this process) gets a real, working provider so its OWN participation
	// succeeds; core-b never runs one, which is what proves the round - not
	// the count - is what decides the outcome.
	rdb := multiCoreRedis(t, "core-a", "core-b")
	s := &AppState{
		Redis: rdb,
		Store: &coreStorageFakeStore{values: map[string]string{}},
		StorageReach: storagereach.NewService(storagereach.ServiceDeps{
			Redis: rdb, CoreID: "core-a", NewProvider: sharedLocalFactory(t.TempDir()),
		}),
	}
	s.reachRoundDeadline = 150 * time.Millisecond

	err := s.checkSharedStorageReachable(context.Background(),
		CoreStorageConfig{Backend: "path", Path: t.TempDir()})

	if err == nil {
		t.Fatal("the save was allowed although core-b never proved access")
	}
	var refusal *sharedStorageRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %T, want *sharedStorageRefusal", err)
	}
	if refusal.status != http.StatusConflict {
		t.Errorf("status = %d, want 409", refusal.status)
	}
	if refusal.result == nil {
		t.Fatal("the refusal carries no RoundResult; the panel cannot name the failing Core")
	}
	found := false
	for _, r := range refusal.result.Results {
		if r.CoreID == "core-b" && r.Status == storagereach.StatusNoResponse {
			found = true
		}
	}
	if !found {
		t.Errorf("results = %+v, want core-b as no-response", refusal.result.Results)
	}
}

func TestCheckSharedStorageReachable_RedisFailureRefuses(t *testing.T) {
	// "Could not verify" must never read as "verified". This mirrors the
	// behaviour the count-based guard already had.
	s := &AppState{Store: &coreStorageFakeStore{values: map[string]string{}}} // no Redis
	err := s.checkSharedStorageReachable(context.Background(),
		CoreStorageConfig{Backend: "path", Path: "/mnt/shared"})
	if err == nil {
		t.Fatal("a save was allowed while the online-Core set could not be read")
	}
}

// TestCheckSharedStorageReachable_PerKeyRedisFailureRefuses closes the gap the
// test above cannot see. There the whole Redis is gone, so the SCAN itself
// fails; here the SCAN succeeds and only the per-key GETs fail - a connection
// reset mid-walk (a failover, a CLIENT KILL, a TCP reset).
//
// Swallowing those GET errors returned an EMPTY list with a NIL error, which
// this function's own "0 and 1 are both not-more-than-one" short-circuit reads
// as nothing to prove: no round runs at all, the new backend is persisted, and
// the panel reports success for a fleet nothing verified. Three Cores are
// seeded precisely so that a correct read would NOT short-circuit, which is
// what makes the empty list the whole failure.
func TestCheckSharedStorageReachable_PerKeyRedisFailureRefuses(t *testing.T) {
	rdb, mr := multiCoreRedisServer(t, "core-a", "core-b", "core-c")
	mr.Server().SetPreHook(func(peer *server.Peer, cmd string, args ...string) bool {
		if cmd == "GET" {
			peer.WriteError("SIMULATED: io: read/write on closed pipe")
			return true
		}
		return false
	})
	s := &AppState{
		Redis: rdb,
		Store: &coreStorageFakeStore{values: map[string]string{}},
		StorageReach: storagereach.NewService(storagereach.ServiceDeps{
			Redis: rdb, CoreID: "core-a", NewProvider: sharedLocalFactory(t.TempDir()),
		}),
		reachRoundDeadline: 150 * time.Millisecond,
	}

	err := s.checkSharedStorageReachable(context.Background(),
		CoreStorageConfig{Backend: "path", Path: t.TempDir()})

	if err == nil {
		t.Fatal("the save was allowed while the online-Core set could not be read: an empty list with a nil error skipped the round entirely and would have persisted an unverified backend")
	}
	var refusal *sharedStorageRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %T, want *sharedStorageRefusal", err)
	}
	if refusal.status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", refusal.status)
	}
}

// TestNewReachProvider_IgnoresTheLiveHostPathGate pins half of why the reach
// provider is built RAW. The gate watches the LIVE configured path; a round
// tests a CANDIDATE one. Consulting it means an admin whose current path has
// wedged cannot point the config at a healthy path to fix it, because the
// provider for the healthy path refuses to build.
func TestNewReachProvider_IgnoresTheLiveHostPathGate(t *testing.T) {
	candidate := t.TempDir()
	s := &AppState{StorageGate: unhealthyGate(t), StorageS3: storage.NewS3Resilience()}

	prov, err := s.NewReachProvider(storagereach.Config{Backend: "path", Path: candidate})
	if err != nil {
		t.Fatalf("NewReachProvider behind an unhealthy LIVE gate: %v, want a healthy CANDIDATE path to build anyway", err)
	}
	if err := prov.WriteFile(context.Background(), "probe.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("WriteFile through the reach provider: %v, want the candidate path usable", err)
	}
}

// TestNewReachProvider_DoesNotTouchTheLiveS3State pins the other half. Going
// through the live resilience wrapper meant a mistyped endpoint flipped the
// LIVE, working backend into reconnecting - which then parks the next call in a
// ten-MINUTE retry budget while the admin's request holds the round lock, and
// reports a firewalled Core as no-response instead of unreachable.
//
// 127.0.0.1:1 is refused immediately, so this is a deterministic
// connection-class failure - the exact class isS3ConnectionClass acts on. A
// context deadline would NOT do: isS3ConnectionClass deliberately returns false
// for one, so a bounded-context version of this test would pass even on the
// broken wiring. AWS_MAX_ATTEMPTS keeps the SDK from spending ~4s on its own
// backoff before handing that refusal back.
func TestNewReachProvider_DoesNotTouchTheLiveS3State(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	s := &AppState{StorageGate: storage.NewGate(), StorageS3: storage.NewS3Resilience()}

	prov, err := s.NewReachProvider(storagereach.Config{
		Backend: "s3", S3Endpoint: "http://127.0.0.1:1", S3Bucket: "candidate",
		S3AccessKey: "AK", S3SecretKey: "SK", S3PathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewReachProvider: %v", err)
	}
	// The raw provider is the exported concrete type; the resilience wrapper is
	// an unexported one, so this alone names which of the two came back.
	if _, ok := prov.(*storage.S3Provider); !ok {
		t.Fatalf("reach provider = %T, want a raw *storage.S3Provider; the live resilience wrapper makes a mistyped endpoint pause for up to ten minutes", prov)
	}
	if err := prov.WriteFile(context.Background(), "probe.txt", strings.NewReader("x")); err == nil {
		t.Fatal("the candidate write against a refused endpoint succeeded; the test setup is broken")
	}

	if reconnecting, _, lastErr := s.StorageS3.State(); reconnecting {
		t.Fatalf("a CANDIDATE config's failure put the LIVE s3 backend into reconnecting (lastErr = %v); the live backend was never tested and may be perfectly healthy", lastErr)
	}
}

// TestCheckSharedStorageReachable_ReleasesTheLockWhenTheProbeWedges is the
// handler-side half of storagereach's own watchdog test: the coordinator's
// probe runs INLINE on this goroutine, which holds both the local mutex and the
// cluster-wide Redis lock. If it never returns, neither lock is ever released
// and every later save on this replica answers 409 "wait for it to finish"
// forever - for a round that will never finish.
//
// blockingProvider's release channel is deliberately never closed here, unlike
// in the concurrency test above.
func TestCheckSharedStorageReachable_ReleasesTheLockWhenTheProbeWedges(t *testing.T) {
	rdb := multiCoreRedis(t, "core-a", "core-b")
	prov := newBlockingProvider()
	s := &AppState{
		Redis: rdb,
		Store: &coreStorageFakeStore{values: map[string]string{}},
		StorageReach: storagereach.NewService(storagereach.ServiceDeps{
			Redis: rdb, CoreID: "core-a",
			NewProvider: func(storagereach.Config) (storage.StorageProvider, error) { return prov, nil },
		}),
		reachRoundDeadline: 150 * time.Millisecond,
	}

	done := make(chan error, 1)
	go func() {
		done <- s.checkSharedStorageReachable(context.Background(),
			CoreStorageConfig{Backend: "path", Path: "/mnt/shared"})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the save was allowed although this Core's own probe never came back")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checkSharedStorageReachable never returned against a wedged backend: the round holds the config-round lock forever and every later save answers 409")
	}

	if _, err := rdb.Get(context.Background(), reachRoundLockKey).Result(); err != redis.Nil {
		t.Errorf("the config-round lock was not released after the wedged round gave up (err = %v)", err)
	}
	// The local mutex has to be free too, or the next save on this replica is
	// refused by the TryLock branch instead.
	if !s.reachRoundMu.TryLock() {
		t.Fatal("the local round mutex is still held after the wedged round gave up")
	}
	s.reachRoundMu.Unlock()
}

// blockingProviderFactory models a NewProvider FACTORY that itself wedges -
// the gap Fix 1 closes, and the discriminating case blockingProvider above
// cannot exercise: there NewProvider itself returns instantly and only a
// provider METHOD blocks, which the round's watchdog already covered before
// Fix 1. Here the factory call never returns at all, modelling
// newStorageProviderForConfig's ungated os.MkdirAll (NewReachProvider always
// builds a candidate provider with a nil gate) hanging on a dead mount before
// any provider exists to call a method on.
type blockingProviderFactory struct {
	entered chan struct{}
	release chan struct{}
}

func newBlockingProviderFactory() *blockingProviderFactory {
	return &blockingProviderFactory{entered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (f *blockingProviderFactory) build(storagereach.Config) (storage.StorageProvider, error) {
	f.entered <- struct{}{}
	<-f.release
	return nil, errors.New("provider build released")
}

// TestCheckSharedStorageReachable_ReleasesTheLockWhenTheProviderBuildWedges is
// Fix 1's discriminating proof at the handler layer: with the provider BUILD
// back outside probeSelf's watchdog (i.e. Fix 1 reverted), this test hangs or
// times out instead of observing the lock released - see
// TestRunRound_ReturnsWithinItsBudgetWhenItsOwnProviderBuildWedges in
// round_test.go for the same proof against RunRound directly.
func TestCheckSharedStorageReachable_ReleasesTheLockWhenTheProviderBuildWedges(t *testing.T) {
	rdb := multiCoreRedis(t, "core-a", "core-b")
	factory := newBlockingProviderFactory()
	s := &AppState{
		Redis: rdb,
		Store: &coreStorageFakeStore{values: map[string]string{}},
		StorageReach: storagereach.NewService(storagereach.ServiceDeps{
			Redis: rdb, CoreID: "core-a", NewProvider: factory.build,
		}),
		reachRoundDeadline: 150 * time.Millisecond,
	}

	done := make(chan error, 1)
	go func() {
		done <- s.checkSharedStorageReachable(context.Background(),
			CoreStorageConfig{Backend: "path", Path: "/mnt/shared"})
	}()

	select {
	case <-factory.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the round never reached the hung provider build; the test setup is broken")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the save was allowed although this Core's own provider never finished building")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checkSharedStorageReachable never returned against a provider build that never returns: the round holds the config-round lock forever and every later save answers 409")
	}

	if _, err := rdb.Get(context.Background(), reachRoundLockKey).Result(); err != redis.Nil {
		t.Errorf("the config-round lock was not released after the wedged provider build gave up (err = %v)", err)
	}
	// The local mutex has to be free too, or the next save on this replica is
	// refused by the TryLock branch instead.
	if !s.reachRoundMu.TryLock() {
		t.Fatal("the local round mutex is still held after the wedged provider build gave up")
	}
	s.reachRoundMu.Unlock()

	close(factory.release)
}

// TestCheckSharedStorageReachable_RefusesWhileAnotherRoundIsLocked is carry-over
// check #2: RunRound's own cleanup (round.go) deletes the single global
// "current round" Redis key unconditionally, without checking it still points
// at ITS round. Two rounds racing would let one delete the other's still-open
// round out from under it. The lock this test seeds simulates a round already
// in flight - from this Core, or (just as easily) from a different Core
// replica the load balancer sent a second concurrent save to - and proves a
// second round refuses outright instead of racing the first.
func TestCheckSharedStorageReachable_RefusesWhileAnotherRoundIsLocked(t *testing.T) {
	rdb := multiCoreRedis(t, "core-a", "core-b")
	if err := rdb.SetNX(context.Background(), reachRoundLockKey, "1", time.Minute).Err(); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	s := &AppState{
		Redis: rdb,
		Store: &coreStorageFakeStore{values: map[string]string{}},
		StorageReach: storagereach.NewService(storagereach.ServiceDeps{
			Redis: rdb, CoreID: "core-a", NewProvider: sharedLocalFactory(t.TempDir()),
		}),
	}

	err := s.checkSharedStorageReachable(context.Background(),
		CoreStorageConfig{Backend: "path", Path: t.TempDir()})

	if err == nil {
		t.Fatal("the save proceeded while another round was already locked")
	}
	var refusal *sharedStorageRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %T, want *sharedStorageRefusal", err)
	}
	if refusal.status != http.StatusConflict {
		t.Errorf("status = %d, want 409", refusal.status)
	}
}

// TestCheckSharedStorageReachable_ReleasesTheLockAfterARound closes the loop on
// the lock above: a round that finishes - success or a fail-closed timeout -
// must not leave the cluster-wide lock behind, or every later save would
// refuse forever.
func TestCheckSharedStorageReachable_ReleasesTheLockAfterARound(t *testing.T) {
	rdb := multiCoreRedis(t, "core-a", "core-b")
	s := &AppState{
		Redis: rdb,
		Store: &coreStorageFakeStore{values: map[string]string{}},
		StorageReach: storagereach.NewService(storagereach.ServiceDeps{
			Redis: rdb, CoreID: "core-a", NewProvider: sharedLocalFactory(t.TempDir()),
		}),
		reachRoundDeadline: 150 * time.Millisecond,
	}

	_ = s.checkSharedStorageReachable(context.Background(), CoreStorageConfig{Backend: "path", Path: t.TempDir()})

	if _, err := rdb.Get(context.Background(), reachRoundLockKey).Result(); err != redis.Nil {
		t.Errorf("the config-round lock was not released after the round finished (err = %v)", err)
	}
}

// TestStorageReachRoundLockRelease_StaleHolderCannotDeleteANewerLock pins the
// compare-and-delete fix: the lock value is a per-attempt token, not a shared
// constant, so a holder releasing after its own TTL has already expired must
// not delete a DIFFERENT holder's lock that has since taken the key. A plain
// unconditional Del would delete whatever is there, deleting a later round's
// still-open lock out from under it - exactly the unconditional-delete bug
// class this lock exists to defend against in RunRound's own cleanup.
func TestStorageReachRoundLockRelease_StaleHolderCannotDeleteANewerLock(t *testing.T) {
	rdb := multiCoreRedis(t, "core-a")
	ctx := context.Background()

	// Simulate a newer holder already owning the key - as if the stale
	// holder's TTL expired, a later round acquired the lock with its own
	// token, and only THEN does the stale holder's deferred release finally
	// run.
	if err := rdb.Set(ctx, reachRoundLockKey, "newer-token", time.Minute).Err(); err != nil {
		t.Fatalf("seed newer lock: %v", err)
	}

	if err := releaseReachRoundLock(ctx, rdb, "stale-token"); err != nil {
		t.Fatalf("releaseReachRoundLock: %v", err)
	}

	got, err := rdb.Get(ctx, reachRoundLockKey).Result()
	if err != nil {
		t.Fatalf("the newer lock is gone after a stale-token release (err = %v); want it untouched", err)
	}
	if got != "newer-token" {
		t.Fatalf("lock value = %q, want %q untouched", got, "newer-token")
	}
}

// TestStorageReachRoundLockRelease_MatchingTokenDeletes is the positive
// control for the test above: a release presenting the SAME token the lock
// was set with must still actually free it, or every save would refuse
// forever.
func TestStorageReachRoundLockRelease_MatchingTokenDeletes(t *testing.T) {
	rdb := multiCoreRedis(t, "core-a")
	ctx := context.Background()

	if err := rdb.Set(ctx, reachRoundLockKey, "my-token", time.Minute).Err(); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	if err := releaseReachRoundLock(ctx, rdb, "my-token"); err != nil {
		t.Fatalf("releaseReachRoundLock: %v", err)
	}
	if _, err := rdb.Get(ctx, reachRoundLockKey).Result(); err != redis.Nil {
		t.Errorf("lock still present after a matching-token release (err = %v)", err)
	}
}

// blockingProvider models a wedged mount for the concurrency test below:
// WriteFile blocks until the test releases it, exactly the RunRound -> Probe
// -> prov.WriteFile chain Fix 1 is about (storage/limiter.go's Gate.Run runs
// WriteFile INLINE with no deadline, so a real wedged mount blocks this call
// forever).
//
// storage.StorageProvider is embedded nil on purpose: any method other than
// the two implemented below is one this fake never expects to be called, and
// a nil-pointer panic says so loudly instead of quietly succeeding.
type blockingProvider struct {
	storage.StorageProvider
	// entered is signalled once WriteFile is called, so the test can wait for
	// the round to actually be inside the hung call before it measures how
	// fast a second, concurrent call refuses.
	entered chan struct{}
	release chan struct{}
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{entered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (p *blockingProvider) WriteFile(context.Context, string, io.Reader) error {
	p.entered <- struct{}{}
	<-p.release
	return errors.New("storage released")
}

// ListFiles is reached once WriteFile finally returns, so it has to answer;
// erroring here keeps Probe's retry loop away from the nil embedded provider.
func (p *blockingProvider) ListFiles(context.Context, string) ([]storage.FileInfo, error) {
	return nil, errors.New("storage released")
}

// DeletePath is reached by RunRound's unconditional CleanupRound defer once
// the round finishes, success or not; a no-op keeps that away from the nil
// embedded provider too.
func (p *blockingProvider) DeletePath(context.Context, string) error {
	return nil
}

// TestCheckSharedStorageReachable_SecondConcurrentCallIsRefusedNotQueued pins
// Fix 1: with a real round wedged inside RunRound (a hung WriteFile, the
// documented failure mode of Gate.Run), a second concurrent call must be
// refused immediately with the same 409 the Redis lock branch returns, not
// queued behind the first until it eventually gives up. Restoring
// reachRoundMu.Lock() in place of TryLock() turns this test into a hang,
// which is the discriminating proof that TryLock is what fixes it.
func TestCheckSharedStorageReachable_SecondConcurrentCallIsRefusedNotQueued(t *testing.T) {
	rdb := multiCoreRedis(t, "core-a", "core-b")
	prov := newBlockingProvider()
	s := &AppState{
		Redis: rdb,
		Store: &coreStorageFakeStore{values: map[string]string{}},
		StorageReach: storagereach.NewService(storagereach.ServiceDeps{
			Redis: rdb, CoreID: "core-a",
			NewProvider: func(storagereach.Config) (storage.StorageProvider, error) { return prov, nil },
		}),
		reachRoundDeadline: 150 * time.Millisecond,
	}

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- s.checkSharedStorageReachable(context.Background(),
			CoreStorageConfig{Backend: "path", Path: "/mnt/shared"})
	}()

	select {
	case <-prov.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the first call never reached the hung write; the test setup is broken")
	}

	secondStart := time.Now()
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- s.checkSharedStorageReachable(context.Background(),
			CoreStorageConfig{Backend: "path", Path: "/mnt/shared"})
	}()

	select {
	case err := <-secondDone:
		if elapsed := time.Since(secondStart); elapsed > time.Second {
			t.Fatalf("the second call took %s to return; it queued behind the first instead of refusing immediately", elapsed)
		}
		var refusal *sharedStorageRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("second call err = %T, want *sharedStorageRefusal", err)
		}
		if refusal.status != http.StatusConflict {
			t.Errorf("second call status = %d, want 409", refusal.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second call never returned: it queued behind the first instead of being refused")
	}

	close(prov.release)
	select {
	case err := <-firstDone:
		if err == nil {
			t.Error("the first call should have failed - its own write never succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first call did not finish after being released")
	}
}

// TestRedactReachResults_ClearsDetailOnly pins the pure helper Fix 1 added:
// Detail is wiped, every other field (which the panel's failure list actually
// renders) survives untouched.
func TestRedactReachResults_ClearsDetailOnly(t *testing.T) {
	in := []storagereach.CoreResult{
		{CoreID: "core-a", Status: storagereach.StatusOK},
		{
			CoreID: "core-b", Status: storagereach.StatusUnreachable,
			Detail: "/mnt/nfs/dylaris-shared: permission denied",
		},
		{
			CoreID: "core-c", Status: storagereach.StatusNotShared,
			MissingPeers: []string{"core-a"}, Detail: "some internal detail",
		},
	}
	got := redactReachResults(in)

	for i, r := range got {
		if r.Detail != "" {
			t.Errorf("Results[%d].Detail = %q, want stripped", i, r.Detail)
		}
	}
	if got[0].CoreID != "core-a" || got[0].Status != storagereach.StatusOK {
		t.Errorf("Results[0] = %+v, want core-a/ok untouched", got[0])
	}
	if got[1].CoreID != "core-b" || got[1].Status != storagereach.StatusUnreachable {
		t.Errorf("Results[1] = %+v, want core-b/unreachable untouched apart from Detail", got[1])
	}
	if len(got[2].MissingPeers) != 1 || got[2].MissingPeers[0] != "core-a" {
		t.Errorf("Results[2].MissingPeers = %v, want [core-a] untouched", got[2].MissingPeers)
	}
	// The input slice itself must be untouched: Aggregate's result may be read
	// elsewhere (the refusal body, the fault-list endpoint) after OnProgress
	// runs, and those callers are supposed to keep the real Detail.
	if in[1].Detail == "" {
		t.Error("redactReachResults mutated the input slice in place; the caller's own copy lost its Detail")
	}
}

// TestCheckSharedStorageReachable_SSEProgressNeverCarriesDetail is Fix 1's
// end-to-end proof: a round with a genuinely failing Core (a distinctive,
// obviously-internal error string, same shape as a real mount/S3 error) must
// never let that string ride along on the storagereach.changed SSE event.
// /api/system/events is authenticated with no capability check, so every
// logged-in tenant with a panel session open would otherwise receive it.
func TestCheckSharedStorageReachable_SSEProgressNeverCarriesDetail(t *testing.T) {
	const rawDetail = "/mnt/nfs/dylaris-shared/.dylaris-reachability/probe: permission denied"
	rdb := multiCoreRedis(t, "core-a", "core-b")
	ctx := context.Background()

	sub := rdb.Subscribe(ctx, services.SystemEventsChannel)
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ch := sub.Channel()

	s := &AppState{
		Redis:  rdb,
		Store:  &coreStorageFakeStore{values: map[string]string{}},
		Events: services.NewSystemEventsPublisher(rdb),
		StorageReach: storagereach.NewService(storagereach.ServiceDeps{
			Redis: rdb, CoreID: "core-a",
			NewProvider: func(storagereach.Config) (storage.StorageProvider, error) {
				return nil, errors.New(rawDetail)
			},
		}),
		reachRoundDeadline: 150 * time.Millisecond,
	}

	if err := s.checkSharedStorageReachable(ctx, CoreStorageConfig{Backend: "path", Path: "/mnt/shared"}); err == nil {
		t.Fatal("the round was expected to fail (core-a's own provider never builds)")
	}

	seenAny := false
	for {
		select {
		case msg := <-ch:
			seenAny = true
			if strings.Contains(msg.Payload, rawDetail) {
				t.Fatalf("published SSE payload leaks the raw backend error text: %s", msg.Payload)
			}
			if strings.Contains(msg.Payload, `"detail"`) {
				t.Fatalf("published SSE payload carries a detail field at all: %s", msg.Payload)
			}
		case <-time.After(200 * time.Millisecond):
			if !seenAny {
				t.Fatal("no storagereach.changed event was published; the test setup is broken")
			}
			return
		}
	}
}
