package storageapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/handlers"
	"dylaris-core/services/storagereach"
	"dylaris-core/storage"
)

func TestStorageReachStatusHandler_ReturnsFaults(t *testing.T) {
	rdb := multiCoreRedis(t, "core-a")
	ctx := context.Background()
	if err := storagereach.RecordFault(ctx, rdb, storagereach.Fault{
		CoreID: "core-b", Status: storagereach.StatusNotShared,
		MissingPeers: []string{"core-a"}, Since: 1, At: 1,
	}); err != nil {
		t.Fatalf("RecordFault: %v", err)
	}
	h := NewCoreStorageHandler(&handlers.AppState{Redis: rdb, Store: &coreStorageFakeStore{values: map[string]string{}}})

	rec := httptest.NewRecorder()
	h.StorageReachStatus(rec, httptest.NewRequest("GET", "/api/settings/storage-reach", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Faults []storagereach.Fault `json:"faults"`
		Online []string             `json:"onlineCores"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	if len(body.Faults) != 1 || body.Faults[0].CoreID != "core-b" {
		t.Fatalf("faults = %+v, want one for core-b", body.Faults)
	}
	if len(body.Online) != 1 || body.Online[0] != "core-a" {
		t.Errorf("onlineCores = %v, want [core-a]", body.Online)
	}
}

// TestStorageReachStatusHandler_DoesNotSelfCheckOrStartARound pins the
// read-only doc comment on StorageReachStatus with a REAL, started verifier
// service rather than trusting the comment alone: Service.SelfCheck is
// documented as unsafe to call concurrently with the service's own running
// Start loop (both end in apply, whose state is single-writer by convention,
// not mutex-guarded), and a round must only ever be started by an actual
// config save. If a future edit ever wires the handler into either path, this
// fails.
func TestStorageReachStatusHandler_DoesNotSelfCheckOrStartARound(t *testing.T) {
	// Two online Cores, not one: checkSharedStorageReachable treats "not more
	// than one online Core" as nothing-to-prove and returns immediately. With
	// only one Core seeded, a regression that wired the status handler into
	// that round check would never even reach the round machinery, and every
	// assertion below would still pass on a broken handler. This reads through
	// services.OnlineCoreIDs (the real Redis heartbeat set), independently of
	// the ServiceDeps.OnlineCores fake below.
	rdb := multiCoreRedis(t, "core-a", "core-b")
	counters := &storageReachDepsCounters{}
	providerRoot := t.TempDir()
	svc := storagereach.NewService(storagereach.ServiceDeps{
		Redis:  rdb,
		CoreID: "core-a",
		// Unconfigured storage keeps the boot self-check a fast no-op (observe
		// returns immediately without touching the provider or Redis, see
		// service.go) so the baseline snapshot below is not racing real
		// filesystem I/O. Counted anyway: ConfigFor is called unconditionally
		// as observe's first step, configured or not, so the counter alone
		// (not what it returns) is what proves whether SelfCheck ran again.
		ConfigFor: func() (storagereach.Config, bool) {
			counters.configFor.Add(1)
			return storagereach.Config{}, false
		},
		OnlineCores: func(ctx context.Context) ([]string, error) {
			counters.onlineCores.Add(1)
			return []string{"core-a"}, nil
		},
		NewProvider: func(cfg storagereach.Config) (storage.StorageProvider, error) {
			counters.newProvider.Add(1)
			return &storage.LocalProvider{BasePath: providerRoot}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc.Start(ctx)

	// Give the boot self-check goroutine time to run and settle so the
	// baseline below isn't racing it - a race there would be a false positive
	// for "the handler changed nothing".
	time.Sleep(50 * time.Millisecond)
	statusBefore, detailBefore := svc.Status().Snapshot()
	configForBefore := counters.configFor.Load()
	onlineCoresBefore := counters.onlineCores.Load()
	newProviderBefore := counters.newProvider.Load()

	h := NewCoreStorageHandler(&handlers.AppState{
		Redis: rdb, Store: &coreStorageFakeStore{values: map[string]string{}}, StorageReach: svc,
	})
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.StorageReachStatus(rec, httptest.NewRequest("GET", "/api/settings/storage-reach", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200 (%s)", i, rec.Code, rec.Body.String())
		}
	}

	statusAfter, detailAfter := svc.Status().Snapshot()
	if statusAfter != statusBefore || detailAfter != detailBefore {
		t.Fatalf("status endpoint changed the local verdict: before = %s/%q, after = %s/%q",
			statusBefore, detailBefore, statusAfter, detailAfter)
	}

	if got := counters.configFor.Load(); got != configForBefore {
		t.Fatalf("ConfigFor was called %d time(s) by the status endpoint (baseline %d): the handler ran a self-check",
			got-configForBefore, configForBefore)
	}
	if got := counters.onlineCores.Load(); got != onlineCoresBefore {
		t.Fatalf("OnlineCores was called %d time(s) by the status endpoint (baseline %d): the handler ran a self-check or a round",
			got-onlineCoresBefore, onlineCoresBefore)
	}
	if got := counters.newProvider.Load(); got != newProviderBefore {
		t.Fatalf("NewProvider was called %d time(s) by the status endpoint (baseline %d): the handler opened a storage provider, meaning it ran a self-check or started a round",
			got-newProviderBefore, newProviderBefore)
	}

	pending, err := storagereach.PendingRoundID(ctx, rdb)
	if err != nil {
		t.Fatalf("PendingRoundID: %v", err)
	}
	if pending != "" {
		t.Fatalf("a round was started by a status-only read: pending round id = %q", pending)
	}
}
