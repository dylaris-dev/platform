package corestorage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"dylaris-core/storage"
)

// unhealthyGate returns a gate that has already been tripped, without starting
// a watcher: ReportFailure is the immediate-trip path and needs no probe loop.
func unhealthyGate(t *testing.T) *storage.Gate {
	t.Helper()
	g := storage.NewGate()
	g.ReportFailure(&os.PathError{Op: "stat", Path: "/mnt/shared", Err: syscall.EIO})
	if ok, _ := g.Healthy(); ok {
		t.Fatal("gate did not trip on an unreachable errno")
	}
	return g
}

// TestNewStorageProviderForConfigChecksGateBeforeMkdirAll pins the ORDERING,
// which is the entire point of putting the check here. NewProviderForConfig
// runs on every provider build and providers are built per request, so on a
// mount that has stopped answering its os.MkdirAll is itself the call that
// blocks - before any provider method is ever reached. The proof is that the
// directory does not exist afterwards: had the check come after, MkdirAll would
// have run (and, on a real wedged mount, hung) first.
func TestNewStorageProviderForConfigChecksGateBeforeMkdirAll(t *testing.T) {
	base := filepath.Join(t.TempDir(), "wedged")
	cfg := Config{Backend: "path", Path: base, PathConfirmed: true}

	prov, err := NewProviderForConfig(cfg, PrefixLibrary, unhealthyGate(t), nil)
	if err == nil {
		t.Fatal("NewProviderForConfig err = nil behind an unhealthy gate, want an error")
	}
	if prov != nil {
		t.Fatalf("NewProviderForConfig = %T, want a nil provider on error", prov)
	}
	if !errors.Is(err, storage.ErrBackendUnreachable) {
		t.Errorf("err = %v, want it to match storage.ErrBackendUnreachable", err)
	}
	if _, statErr := os.Stat(base); !os.IsNotExist(statErr) {
		t.Fatalf("%q exists, so MkdirAll ran despite an unhealthy gate; the check must come BEFORE it", base)
	}
}

func TestNewStorageProviderForConfigHealthyGateBuildsGatedProvider(t *testing.T) {
	base := t.TempDir()
	cfg := Config{Backend: "path", Path: base, PathConfirmed: true}

	prov, err := NewProviderForConfig(cfg, PrefixLibrary, storage.NewGate(), nil)
	if err != nil {
		t.Fatalf("NewProviderForConfig: %v", err)
	}
	want := filepath.Join(base, PrefixLibrary)
	if _, statErr := os.Stat(want); statErr != nil {
		t.Errorf("root %q was not created: %v", want, statErr)
	}
	lp, ok := storage.UnwrapGated(prov).(*storage.LocalProvider)
	if !ok {
		t.Fatalf("underlying provider = %T, want *storage.LocalProvider", storage.UnwrapGated(prov))
	}
	if lp.BasePath != want {
		t.Errorf("BasePath = %q, want %q", lp.BasePath, want)
	}
}

// TestNewStorageProviderForConfigNeverGatesS3: the watchdog watches ONE host
// path. S3 has no relationship to it, and its failures are the SDK's to judge.
func TestNewStorageProviderForConfigNeverGatesS3(t *testing.T) {
	cfg := Config{
		Backend:     "s3",
		S3Bucket:    "my-bucket",
		S3AccessKey: "AKIAEXAMPLE",
		S3SecretKey: "s3cr3t",
	}
	prov, err := NewProviderForConfig(cfg, PrefixLibrary, unhealthyGate(t), nil)
	if err != nil {
		t.Fatalf("s3 build behind an unhealthy path gate: %v, want it unaffected", err)
	}
	if prov == nil {
		t.Fatal("s3 build returned a nil provider")
	}
	if storage.UnwrapGated(prov) != prov {
		t.Error("the s3 provider came back wrapped by the host-path gate")
	}
}

// TestBuildCoreStorageProviderSurfacesUnreachable drives the same thing through
// the per-request entry point every handler actually calls.
func TestBuildCoreStorageProviderSurfacesUnreachable(t *testing.T) {
	const cfgPath = "/mnt/shared-that-is-never-created"
	s := &Manager{
		Store: &coreStorageFakeStore{values: map[string]string{
			KeyBackend:     "path",
			KeyPath:        cfgPath,
			KeyPathConfirm: "true",
		}},
		Gate: unhealthyGate(t),
	}

	prov, err := s.BuildProvider(PrefixLibrary)
	if err == nil || prov != nil {
		t.Fatalf("BuildProvider = (%T, %v), want (nil, an error)", prov, err)
	}
	if !errors.Is(err, storage.ErrBackendUnreachable) {
		t.Errorf("err = %v, want it to match storage.ErrBackendUnreachable", err)
	}
	if _, statErr := os.Stat(cfgPath); !os.IsNotExist(statErr) {
		t.Errorf("%q exists; nothing may be created on the wedged path", cfgPath)
	}
}

// TestSyncStorageGateFollowsTheConfiguredBackend: a verdict about a host path
// must not keep refusing requests once the platform stopped using that path.
func TestSyncStorageGateFollowsTheConfiguredBackend(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
	}{
		{"switched to s3", map[string]string{
			KeyBackend:  "s3",
			KeyS3Bucket: "my-bucket",
		}},
		{"nothing configured", map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Manager{
				Store: &coreStorageFakeStore{values: tt.values},
				Gate:  unhealthyGate(t),
			}
			s.SyncGate()
			if ok, cause := s.Gate.Healthy(); !ok {
				t.Errorf("Healthy() = (false, %v); a stale host-path verdict must not survive %s", cause, tt.name)
			}
		})
	}
}

// TestSyncStorageGateIsNilSafe: an Manager built without a gate (only ever a
// test) must take the same path as the wired-up one rather than panic.
func TestSyncStorageGateIsNilSafe(t *testing.T) {
	s := &Manager{Store: &coreStorageFakeStore{values: map[string]string{
		KeyBackend:     "path",
		KeyPath:        "/mnt/shared",
		KeyPathConfirm: "true",
	}}}
	s.SyncGate()
	if ok, _ := s.Gate.Healthy(); !ok {
		t.Error("a nil gate must read as healthy")
	}
}

// TestNewStorageProviderForConfigBoundsTheSlotWait proves the build passes a
// BOUNDED context to the gate, not context.Background(). With every filesystem
// slot held, the MkdirAll acquire can never proceed; a bounded wait fails the
// build with a deadline error, while context.Background() would block the
// goroutine forever - undrainable at shutdown, unreactive to a client hangup.
//
// A healthy gate is used deliberately: GatedProviderBlocked passes, so the only
// thing that can stop the build is the acquire deadline itself.
func TestNewStorageProviderForConfigBoundsTheSlotWait(t *testing.T) {
	gate := storage.NewGate()

	// Saturate every slot with holders that stay inside the gate until released.
	release := make(chan struct{})
	var held sync.WaitGroup
	entered := make(chan struct{}, storage.MaxConcurrentFSOps)
	for i := 0; i < storage.MaxConcurrentFSOps; i++ {
		held.Add(1)
		go func() {
			defer held.Done()
			_ = gate.Run(context.Background(), func() error {
				entered <- struct{}{}
				<-release
				return nil
			})
		}()
	}
	for i := 0; i < storage.MaxConcurrentFSOps; i++ {
		<-entered // all slots now taken
	}
	t.Cleanup(func() { close(release); held.Wait() })

	// Shrink the wait so the test does not sit for the real 15s.
	prev := storageSlotWaitDeadline
	storageSlotWaitDeadline = 100 * time.Millisecond
	t.Cleanup(func() { storageSlotWaitDeadline = prev })

	cfg := Config{Backend: "path", Path: t.TempDir(), PathConfirmed: true}

	done := make(chan error, 1)
	go func() {
		_, err := NewProviderForConfig(cfg, PrefixLibrary, gate, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("build succeeded with every slot held; want a deadline error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("build never returned with every slot held; the acquire wait is not bounded (context.Background()?)")
	}
}
