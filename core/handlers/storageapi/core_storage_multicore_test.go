package storageapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/handlers"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/services"
	"dylaris-core/services/storagereach"
	"dylaris-core/storage"
	"dylaris-core/store"
)

// These cover the single-Core constraint on the filesystem backend: a host
// path stores files on ONE machine's disk, so with a second Core online each
// instance serves only what it wrote itself - and nothing fails loudly,
// because every Core reads its own writes back perfectly.

// multiCoreFakeStore serves settings from a map and records writes, so a test
// can assert that a refused save persisted nothing.
type multiCoreFakeStore struct {
	store.Store
	values map[string]string
	writes int
}

func (f *multiCoreFakeStore) GetSetting(key string) (string, error) { return f.values[key], nil }

func (f *multiCoreFakeStore) SetSetting(key, value string) error {
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[key] = value
	f.writes++
	return nil
}

func multiCoreState(t *testing.T, values map[string]string) *multiCoreFakeStore {
	t.Helper()
	return &multiCoreFakeStore{values: values}
}

// multiCoreRedis returns a client whose keyspace holds exactly the heartbeats
// named by ids, written the way CoreHeartbeatService writes them. No ids means
// a live Redis with an empty keyspace; use downRedis for the unreachable case.
func multiCoreRedis(t *testing.T, ids ...string) *redis.Client {
	t.Helper()
	rdb, _ := multiCoreRedisServer(t, ids...)
	return rdb
}

// multiCoreRedisServer is multiCoreRedis plus the server behind it, for the one
// test that has to inject a per-command fault (SetPreHook) rather than just
// seed keys.
func multiCoreRedisServer(t *testing.T, ids ...string) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	for _, id := range ids {
		data, err := json.Marshal(services.CoreHeartbeat{ID: id})
		if err != nil {
			t.Fatalf("marshal heartbeat: %v", err)
		}
		mr.Set("dylaris:core:"+id, string(data))
	}
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

// downRedis returns a client pointed at a server that has been shut down, so
// every command fails.
func downRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	addr := mr.Addr()
	mr.Close()
	return redis.NewClient(&redis.Options{Addr: addr})
}

// sharedLocalFactory hands every participant a LocalProvider on the same
// root, mirroring services/storagereach/round_test.go's sharedFactory: what a
// genuinely shared mount looks like from the verifier's point of view. It
// ignores cfg entirely (backend, path, s3 fields) on purpose - the tests below
// are about SaveConfig's wiring of the round, not about proving a real
// filesystem or S3 endpoint, exactly like round_test.go's own use of it with
// an s3-flavoured Config.
func sharedLocalFactory(root string) storagereach.ProviderFactory {
	return func(storagereach.Config) (storage.StorageProvider, error) {
		return &storage.LocalProvider{BasePath: root}, nil
	}
}

// TestSaveConfig_RefusesHostPathOnMultipleCores drives the real handler,
// because the guard being correct is worth nothing if the save path does not
// consult it. Two Cores heartbeat but only core-a (the handler under test)
// ever participates in the round, so core-b is fail-closed no-response - the
// same "a peer never proves it" case checkSharedStorageReachable's own unit
// tests cover, exercised here through the actual SaveConfig HTTP handler. It
// asserts nothing was persisted as well as the status: the writes run as an
// unguarded loop, so a refusal landing late would leave a half-applied config
// behind.
func TestSaveConfig_RefusesHostPathOnMultipleCores(t *testing.T) {
	st := multiCoreState(t, map[string]string{})
	rdb := multiCoreRedis(t, "core-a", "core-b")
	state := &handlers.AppState{
		Store: st,
		Redis: rdb,
		StorageReach: storagereach.NewService(storagereach.ServiceDeps{
			Redis: rdb, CoreID: "core-a", NewProvider: sharedLocalFactory(t.TempDir()),
		}),
	}
	state.SetReachRoundDeadline(150 * time.Millisecond)
	h := &CoreStorageHandler{state: state}

	body := `{"backend":"path","path":"/mnt/shared","pathConfirmed":true}`
	rec := httptest.NewRecorder()
	h.SaveConfig(rec, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", strings.NewReader(body)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if st.writes != 0 {
		t.Errorf("the refused save persisted %d settings; it must persist none", st.writes)
	}
}

// TestSaveConfig_AllowsS3OnMultipleCores is the positive control, updated for
// the round-based guard: s3 on multiple Cores is no longer an unconditional
// bypass (the old count-only guard's "s3 is never gated" - it must now pass
// the SAME proof a host path does. core-b runs a real participant goroutine,
// exactly like the service loop does in production, so the round can actually
// confirm both Cores and the save succeeds.
func TestSaveConfig_AllowsS3OnMultipleCores(t *testing.T) {
	st := multiCoreState(t, map[string]string{})
	rdb := multiCoreRedis(t, "core-a", "core-b")
	factory := sharedLocalFactory(t.TempDir())
	state := &handlers.AppState{
		Store:       st,
		Redis:       rdb,
		StorageGate: storage.NewGate(),
		StorageS3:   storage.NewS3Resilience(),
		StorageReach: storagereach.NewService(storagereach.ServiceDeps{
			Redis: rdb, CoreID: "core-a", NewProvider: factory,
		}),
		// The poll cadence is what matters here, not the deadline. It is how
		// often the coordinator retries its OWN storage listing (and, published
		// as PollEveryMillis, how often core-b retries its probe), so together
		// with the deadline it decides how many chances core-a gets to observe
		// core-b's beacon.
		//
		// This test used to run at the production 1s cadence with a 1200ms
		// deadline, which bought exactly TWO listings: core-b's goroutine had a
		// single one-second window to be scheduled, discover the round and
		// write. That held locally and flaked on CI, where the runner executes
		// the whole job matrix at once - core-b wrote after core-a's second and
		// last listing, so core-a reported not-shared while core-b reported ok
		// and the save was refused with confirmed 1 of 2.
		//
		// 50ms over 6s is ~120 listings instead of 2, which removes the
		// dependence on when a goroutine happens to be scheduled rather than
		// just widening the window. It is also faster in the passing case: the
		// round ends as soon as every report is in AND the coordinator's own
		// probe sees all peers, so this now returns in well under a second
		// instead of waiting out a full 1s poll. The deadline only bounds how
		// long a genuinely lost race keeps retrying before failing.
		// 30s, not 6s. The round ends the moment every report is in, so a passing
		// test never waits on this; it only bounds how long a genuinely lost race
		// keeps retrying. 6s was still short enough for a saturated runner to turn
		// a healthy round into "core-b: no-response" and fail the gate on code
		// that was fine - the same lesson settlingRound() in the storagereach
		// package already records. A deadline a passing test never reaches cannot
		// cause a false failure.
	}
	state.SetReachRoundDeadline(30 * time.Second)
	state.SetReachRoundPoll(50 * time.Millisecond)
	h := &CoreStorageHandler{state: state}

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

	body := `{"backend":"s3","s3Bucket":"b","s3AccessKey":"AKIA","s3SecretKey":"secret"}`
	rec := httptest.NewRecorder()
	h.SaveConfig(rec, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", strings.NewReader(body)))

	if perr := <-done; perr != nil {
		t.Fatalf("core-b participant: %v", perr)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if st.values[handlers.KeyCoreStorageBackend] != "s3" {
		t.Errorf("backend persisted as %q, want %q", st.values[handlers.KeyCoreStorageBackend], "s3")
	}
}

// TestSaveConfig_RefusesWhenTheVerifierIsNotRunning covers the "could not
// verify = refuse" rule at the SaveConfig level: a Core with no StorageReach
// service (e.g. built by tooling, or before boot wiring completes) must not
// let a multi-Core save through just because it cannot check it.
func TestSaveConfig_RefusesWhenTheVerifierIsNotRunning(t *testing.T) {
	st := multiCoreState(t, map[string]string{})
	h := &CoreStorageHandler{state: &handlers.AppState{Store: st, Redis: multiCoreRedis(t, "core-a", "core-b")}}

	body := `{"backend":"s3","s3Bucket":"b","s3AccessKey":"AKIA","s3SecretKey":"secret"}`
	rec := httptest.NewRecorder()
	h.SaveConfig(rec, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", strings.NewReader(body)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if st.writes != 0 {
		t.Errorf("the refused save persisted %d settings; it must persist none", st.writes)
	}
}

// TestGetConfig_ReportsOnlineCores replaces TestGetConfig_ReportsHostPathAvailability:
// the count-based host-path guard is gone (checkSharedStorageReachable proves
// reachability with a real cross-Core round instead), so GetConfig no longer
// computes hostPathAllowed or hostPathWarning from the count - only the count
// itself, which the panel's round-progress counter uses as its expected
// total. The negative assertions guard against those two dead fields quietly
// reappearing.
func TestGetConfig_ReportsOnlineCores(t *testing.T) {
	tests := []struct {
		name            string
		values          map[string]string
		cores           []string
		redisDown       bool
		wantOnlineCores float64
	}{
		{
			name:            "one Core",
			values:          map[string]string{},
			cores:           []string{"core-a"},
			wantOnlineCores: 1,
		},
		{
			name:            "a saved host path on two Cores",
			values:          map[string]string{handlers.KeyCoreStorageBackend: "path", handlers.KeyCoreStoragePath: "/mnt/shared"},
			cores:           []string{"core-a", "core-b"},
			wantOnlineCores: 2,
		},
		{
			name:            "a saved s3 config on two Cores",
			values:          map[string]string{handlers.KeyCoreStorageBackend: "s3", handlers.KeyCoreStorageS3Bucket: "b"},
			cores:           []string{"core-a", "core-b"},
			wantOnlineCores: 2,
		},
		{
			// A hint the server could not compute must not fail the response.
			name:            "an unverifiable count still returns 200",
			values:          map[string]string{},
			redisDown:       true,
			wantOnlineCores: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rdb := multiCoreRedis(t, tc.cores...)
			if tc.redisDown {
				rdb = downRedis(t)
			}
			h := &CoreStorageHandler{state: &handlers.AppState{Store: multiCoreState(t, tc.values), Redis: rdb}}

			rec := httptest.NewRecorder()
			h.GetConfig(rec, httptest.NewRequest(http.MethodGet, "/api/settings/core-storage", nil))

			var body map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if body["onlineCores"] != tc.wantOnlineCores {
				t.Errorf("onlineCores = %v, want %v", body["onlineCores"], tc.wantOnlineCores)
			}
			if _, present := body["hostPathAllowed"]; present {
				t.Error("response still carries hostPathAllowed; the count-based guard was removed and must not come back")
			}
			if _, present := body["hostPathWarning"]; present {
				t.Error("response still carries hostPathWarning; the count-based warning was removed and must not come back")
			}
		})
	}
}

// TestGetConfig_LeaksNoInstanceIdentities: the ids are hostnames and the count
// is all the form needs, so they must not ride along in the response.
func TestGetConfig_LeaksNoInstanceIdentities(t *testing.T) {
	h := &CoreStorageHandler{state: &handlers.AppState{
		Store: multiCoreState(t, map[string]string{}),
		Redis: multiCoreRedis(t, "core-prod-fra-01", "core-prod-fra-02"),
	}}

	rec := httptest.NewRecorder()
	h.GetConfig(rec, httptest.NewRequest(http.MethodGet, "/api/settings/core-storage", nil))

	for _, id := range []string{"core-prod-fra-01", "core-prod-fra-02"} {
		if strings.Contains(rec.Body.String(), id) {
			t.Errorf("the response named instance %q; body: %s", id, rec.Body.String())
		}
	}
}
