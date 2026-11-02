package corestorage

import (
	"os"
	"path/filepath"
	"testing"

	"dylaris-core/storage"
	"dylaris-core/store"
)

// coreStorageFakeStore returns a per-key canned setting value from an
// internal map. This is distinct from modeFakeStore (permissions_mode_test.go),
// which always returns the same value regardless of the requested key and so
// cannot drive the multi-key Config shape.
type coreStorageFakeStore struct {
	store.Store
	values map[string]string
}

// GetSetting mirrors the real store's behavior for an unset setting: an empty
// value and a nil error.
func (f *coreStorageFakeStore) GetSetting(key string) (string, error) {
	return f.values[key], nil
}

func TestLoadCoreStorageConfig(t *testing.T) {
	cases := []struct {
		name      string
		values    map[string]string
		want      Config
		wantValid bool
	}{
		{
			name:      "all keys empty is unconfigured and invalid",
			values:    map[string]string{},
			want:      Config{},
			wantValid: false,
		},
		{
			name: "valid path config",
			values: map[string]string{
				KeyBackend:     "path",
				KeyPath:        "/mnt/shared",
				KeyPathConfirm: "true",
			},
			want: Config{
				Backend:       "path",
				Path:          "/mnt/shared",
				PathConfirmed: true,
			},
			wantValid: true,
		},
		{
			name: "valid s3 config",
			values: map[string]string{
				KeyBackend:     "s3",
				KeyS3Bucket:    "my-bucket",
				KeyS3AccessKey: "AKIAEXAMPLE",
				KeyS3SecretKey: "s3cr3t",
			},
			want: Config{
				Backend:     "s3",
				S3Bucket:    "my-bucket",
				S3AccessKey: "AKIAEXAMPLE",
				S3SecretKey: "s3cr3t",
				S3SecretSet: true,
			},
			wantValid: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Manager{Store: &coreStorageFakeStore{values: c.values}}
			got := s.LoadConfig()
			if got != c.want {
				t.Fatalf("LoadConfig() = %+v, want %+v", got, c.want)
			}
			if valid := s.Configured(); valid != c.wantValid {
				t.Fatalf("Configured() = %v, want %v", valid, c.wantValid)
			}
		})
	}
}

// firstMissingAncestor walks up from target to find the highest-level
// directory in its ancestor chain that does not yet exist on disk. This is
// the exact directory os.MkdirAll would create as a side effect, so a test
// can remove precisely that (and nothing that pre-existed) during cleanup.
// Returns "" if target and all its ancestors already exist.
func firstMissingAncestor(t *testing.T, target string) string {
	t.Helper()
	abs, err := filepath.Abs(target)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", target, err)
	}
	var chain []string
	for cur := abs; ; {
		chain = append(chain, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if _, err := os.Stat(chain[i]); os.IsNotExist(err) {
			return chain[i]
		}
	}
	return ""
}

var coreStoragePrefixes = []string{
	PrefixLibrary,
	PrefixAttachments,
	PrefixBackups,
}

// TestBuildCoreStorageProvider_UnconfiguredReturnsErrorNoFallback is the core
// regression guard for the removed legacy-directory fallback (there are no
// pre-existing DYLARIS installs anywhere - the fallback existed only to keep
// those readable): with no (or an invalid) shared-storage config,
// BuildProvider must return a nil provider and a non-nil error,
// never silently construct a node-local dylaris_data/<subPrefix> provider.
func TestBuildCoreStorageProvider_UnconfiguredReturnsErrorNoFallback(t *testing.T) {
	for _, prefix := range coreStoragePrefixes {
		t.Run(prefix, func(t *testing.T) {
			s := &Manager{Store: &coreStorageFakeStore{values: map[string]string{}}}
			p, err := s.BuildProvider(prefix)
			if err == nil {
				t.Fatalf("BuildProvider(%q) err = nil, want a non-nil error (no config)", prefix)
			}
			if p != nil {
				t.Fatalf("BuildProvider(%q) = %T, want a nil provider on error", prefix, p)
			}
		})
	}
}

// TestBuildCoreStorageProvider_InvalidReturnsErrorNoFallback covers the other
// unconfigured shape: a stored config that fails validation (e.g. a relative
// path, or a path backend missing the operator confirmation) must also be a
// hard error, never a silent fallback.
func TestBuildCoreStorageProvider_InvalidReturnsErrorNoFallback(t *testing.T) {
	s := &Manager{Store: &coreStorageFakeStore{values: map[string]string{
		KeyBackend: "path",
		KeyPath:    "relative/not/absolute",
		// KeyPathConfirm intentionally omitted too.
	}}}
	p, err := s.BuildProvider(PrefixLibrary)
	if err == nil {
		t.Fatal("BuildProvider err = nil, want a non-nil error (invalid config)")
	}
	if p != nil {
		t.Fatalf("BuildProvider = %T, want a nil provider on error", p)
	}
}

// TestBuildCoreStorageProvider_ConfiguredPathBackendScopesBySubPrefix asserts
// that a configured path backend scopes each subsystem under its own
// subdirectory of the operator-configured shared path, rather than all
// subsystems colliding into one root.
func TestBuildCoreStorageProvider_ConfiguredPathBackendScopesBySubPrefix(t *testing.T) {
	const cfgPath = "/mnt/shared"

	// Same MkdirAll side effect as the unconfigured case, rooted at cfgPath
	// instead of dylaris_data; clean up only what this test freshly creates.
	if root := firstMissingAncestor(t, filepath.Join(cfgPath)); root != "" {
		t.Cleanup(func() { os.RemoveAll(root) })
	}

	for _, prefix := range coreStoragePrefixes {
		t.Run(prefix, func(t *testing.T) {
			s := &Manager{Store: &coreStorageFakeStore{values: map[string]string{
				KeyBackend:     "path",
				KeyPath:        cfgPath,
				KeyPathConfirm: "true",
			}}}
			p, err := s.BuildProvider(prefix)
			if err != nil {
				t.Fatalf("BuildProvider(%q): %v", prefix, err)
			}
			// The path backend is handed back wrapped in the storage-gate
			// wrapper, so unwrap before inspecting the concrete backend.
			if _, wrapped := p.(*storage.LocalProvider); wrapped {
				t.Fatalf("BuildProvider(%q) returned a bare *storage.LocalProvider, want it wrapped by the storage gate", prefix)
			}
			lp, ok := storage.UnwrapGated(p).(*storage.LocalProvider)
			if !ok {
				t.Fatalf("BuildProvider(%q) = %T, want *storage.LocalProvider under the gate wrapper", prefix, storage.UnwrapGated(p))
			}
			want := filepath.Join(cfgPath, prefix)
			if lp.BasePath != want {
				t.Fatalf("BasePath = %q, want %q", lp.BasePath, want)
			}
		})
	}
}

// TestBuildCoreStorageProvider_ConfiguredS3BackendBuildsProvider asserts that
// a valid s3 config builds a (non-local) provider without error. Constructing
// the AWS SDK client from static credentials does not touch the network, so
// this must succeed offline.
func TestBuildCoreStorageProvider_ConfiguredS3BackendBuildsProvider(t *testing.T) {
	s := &Manager{Store: &coreStorageFakeStore{values: map[string]string{
		KeyBackend:     "s3",
		KeyS3Bucket:    "my-bucket",
		KeyS3AccessKey: "AKIAEXAMPLE",
		KeyS3SecretKey: "s3cr3t",
	}}}

	p, err := s.BuildProvider(PrefixLibrary)
	if err != nil {
		t.Fatalf("BuildProvider(s3): %v", err)
	}
	if p == nil {
		t.Fatal("BuildProvider(s3) returned a nil provider")
	}
	if _, ok := p.(*storage.LocalProvider); ok {
		t.Fatal("BuildProvider(s3) returned a *storage.LocalProvider, want an s3 provider")
	}
}
