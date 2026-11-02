package corestorage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dylaris-core/services"
	"dylaris-core/storage"
)

// Setting keys for the single shared "Core file storage" config.
const (
	KeyBackend     = "core_storage_backend"
	KeyPath        = "core_storage_path"
	KeyPathConfirm = "core_storage_path_confirmed"
	KeyS3Endpoint  = "core_storage_s3_endpoint"
	KeyS3Bucket    = "core_storage_s3_bucket"
	KeyS3Region    = "core_storage_s3_region"
	KeyS3AccessKey = "core_storage_s3_access_key"
	KeyS3SecretKey = "core_storage_s3_secret_key"
	KeyS3PathStyle = "core_storage_s3_path_style"
	KeyS3Prefix    = "core_storage_s3_prefix"
)

// Sub-prefixes carve one shared provider into per-subsystem namespaces.
// server-backups and modpacks are OPT-IN namespaces: they are only used when
// an operator explicitly points that subsystem at the "core-storage" backend.
// PrefixServerBackups is duplicated as storage/backup.CoreStorageSubPrefix
// and PrefixModpacks is duplicated as storage/modpack.CoreStorageSubPrefix
// (neither package may import this one); TestCoreStorageSubPrefixesMatch locks
// both pairs together.
//
// PrefixLibrary belongs to the removed Library. It stays so the
// one-time purge (PurgeLibraryStorage) can find what is left there, and so
// nothing new is ever given that namespace.
const (
	PrefixLibrary       = "library"
	PrefixAttachments   = "ticket-attachments"
	PrefixBackups       = "ticket-backups"
	PrefixServerBackups = "server-backups"
	PrefixModpacks      = "modpacks"
	PrefixTemplates     = "templates"
)

// storageSlotWaitDeadline bounds how long a host-path provider build waits for
// a free filesystem-concurrency slot before failing the build. Long enough that
// ordinary contention (a slot freeing as an in-flight op completes) is never
// tripped, short enough to stay well inside the shutdown drain so a build in
// progress cannot hold shutdown open. It bounds only the WAIT; the gate's own
// deadline still bounds the MkdirAll once a slot is held.
//
// A var, not a const, only so a test can shrink it to prove the wait is bounded
// without waiting the real 15s. Production never reassigns it.
var storageSlotWaitDeadline = 15 * time.Second

// Config is the wire + persisted shape of the shared config. The S3
// secret is write-only: never emitted on read; S3SecretSet tells the UI whether
// one is already stored.
type Config struct {
	Backend       string `json:"backend"` // "path" | "s3"
	Path          string `json:"path"`
	PathConfirmed bool   `json:"pathConfirmed"`
	S3Endpoint    string `json:"s3Endpoint"`
	S3Bucket      string `json:"s3Bucket"`
	S3Region      string `json:"s3Region"`
	S3AccessKey   string `json:"s3AccessKey"`
	S3SecretKey   string `json:"s3SecretKey,omitempty"`
	S3PathStyle   bool   `json:"s3PathStyle"`
	S3Prefix      string `json:"s3Prefix"`
	S3SecretSet   bool   `json:"s3SecretSet"`
	// ConnectionID references a saved storage connection. When non-zero, the
	// provider is built from that named connection instead of the inline
	// s3 fields above (see EffectiveConfig). 0 = use the inline
	// config. Echoed back to the panel so the form can show the selection.
	ConnectionID int `json:"connectionId"`
}

// Validate enforces the rules the panel also mirrors: path
// must be absolute AND operator-confirmed; s3 must have bucket + credentials.
func Validate(c Config) error {
	switch c.Backend {
	case "path", "local":
		if c.Path == "" {
			return fmt.Errorf("core storage: path is required for the filesystem backend")
		}
		// The configured path is always a path on the Core container's
		// filesystem (Linux; this ships as a Docker image only), so the
		// absolute-path check is Linux-explicit rather than filepath.IsAbs
		// (which is host-OS-dependent and would treat "/mnt/shared" as
		// relative when the handler itself is built/tested on Windows).
		// Same precedent as storage/modpack.IsUnsafeEntryPath.
		if !strings.HasPrefix(c.Path, "/") {
			return fmt.Errorf("core storage: path must be absolute")
		}
		if !c.PathConfirmed {
			return fmt.Errorf("core storage: the shared-path confirmation is required")
		}
		return nil
	case "s3":
		if c.S3Bucket == "" {
			return fmt.Errorf("core storage: s3 bucket is required")
		}
		if c.S3AccessKey == "" || c.S3SecretKey == "" {
			return fmt.Errorf("core storage: s3 access key + secret are required")
		}
		if err := ValidateS3Endpoint("core storage", c.S3Endpoint); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("core storage: backend must be \"path\" or \"s3\"")
	}
}

// LoadConfig reads the persisted config. S3SecretKey is loaded so
// callers that build a provider have it, but handlers must blank it before
// returning to a client. S3SecretSet reflects whether a secret exists.
func (s *Manager) LoadConfig() Config {
	get := func(k string) string {
		if s.Store == nil {
			return ""
		}
		v, _ := s.Store.GetSetting(k)
		return v
	}
	secret := get(KeyS3SecretKey)
	connID, _ := strconv.Atoi(get(KeyConnectionID)) // "" or bad -> 0 = none
	return Config{
		Backend:       get(KeyBackend),
		Path:          get(KeyPath),
		PathConfirmed: get(KeyPathConfirm) == "true",
		S3Endpoint:    get(KeyS3Endpoint),
		S3Bucket:      get(KeyS3Bucket),
		S3Region:      get(KeyS3Region),
		S3AccessKey:   get(KeyS3AccessKey),
		S3SecretKey:   secret,
		S3PathStyle:   get(KeyS3PathStyle) == "true",
		S3Prefix:      get(KeyS3Prefix),
		S3SecretSet:   secret != "",
		ConnectionID:  connID,
	}
}

// Configured reports whether a valid shared config exists. It uses
// the effective config so a selected storage connection counts as configured
// (and a selected-but-broken connection counts as NOT configured, since
// EffectiveConfig errors on it).
func (s *Manager) Configured() bool {
	cfg, err := s.EffectiveConfig()
	return err == nil && Validate(cfg) == nil
}

// SyncGate points the two storage connection mechanisms at whichever
// backend is currently configured: the watchdog watches the host path, or is
// stopped, and the s3 state is dropped when the backend is not s3.
//
// Both halves matter, because each mechanism can otherwise outlive the backend
// it describes. A gate left watching an abandoned path would keep refusing
// requests that no longer go there; an s3 state left reconnecting after a
// switch to a host path would report an outage forever, since the only thing
// that can clear it is a successful s3 call that will never happen.
//
// It is called at boot and from PersistConfig, which is the single
// writer of these settings keys and therefore the only thing that can change
// the answer. Watch, Stop and Reset are all idempotent, so calling this on a
// config write that changed something else costs nothing.
func (s *Manager) SyncGate() {
	cfg, err := s.EffectiveConfig()
	if err != nil {
		// A selected-but-broken connection is still an s3 intent: stop the path
		// watchdog (we are not on a host path) and leave the s3 state alone. The
		// per-request provider build surfaces the real error to callers.
		s.Gate.Stop()
		return
	}
	if (cfg.Backend == "path" || cfg.Backend == "local") && cfg.Path != "" {
		s.Gate.Watch(cfg.Path)
	} else {
		s.Gate.Stop()
	}
	if cfg.Backend != "s3" {
		s.S3.Reset()
	}
}

// isHostPathBackend recognises the two spellings of the filesystem backend.
// "local" is the historical value; "path" is what the panel writes today. Both
// still appear in stored configs.
//
// It is NOT the only place they are matched, and an earlier version of this
// comment saying so was wrong: Validate, SyncGate,
// TestConnection's ephemeral-path branch and health.go's storageComponent each
// switch on the literals directly. Anyone adding a third spelling has to visit
// all of them - this helper alone will not carry it.
//
// Unused since the count-based host-path warning it backed was removed (a real
// cross-Core round proves sharing now instead of guessing from a count) -
// Kept deliberately: it is the single most direct pointer to the
// literal-matching call sites listed above, and storagereach.isPathBackend's
// own doc comment names it by this exact identifier.
//
//lint:ignore U1000 kept as the named reference for the literal call sites above
func isHostPathBackend(backend string) bool {
	return backend == "path" || backend == "local"
}

// CountOnlineCores reports how many Core instances are currently heartbeating.
//
// Informational only: it backs the "onlineCores" figure GetConfig returns for
// the panel's live X/N round-progress counter. It used to also gate the
// host-path backend by count, but that guess is gone now that
// CheckSharedReachable PROVES reachability with a real cross-Core
// round instead - a genuinely shared host path across many Cores is no longer
// refused (or warned about) before it is even tried.
func (s *Manager) CountOnlineCores(ctx context.Context) (int, error) {
	ids, err := services.OnlineCoreIDs(ctx, s.Redis)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

// ProbeS3Connection performs one cheap, read-only call against the configured
// s3 backend. It is the probe storage.S3Resilience.StartProbe runs while the
// backend is reconnecting, and the only thing that lets an upload-only Core
// notice that the object store came back.
//
// It deliberately builds a RAW provider (nil gate, nil resilience). Going
// through the resilient wrapper would put the probe inside s3Retry, where it
// would pause and retry for the whole budget instead of returning a verdict.
//
// ListFiles, not the write/read/delete probe TestConnection uses: this runs
// every interval for as long as an outage lasts, so it must not generate
// writes, and the state it is clearing is about the transport rather than
// about permissions. A backend that answers a list but refuses a write is
// reachable, which is exactly what the reconnecting state asks about.
func (s *Manager) ProbeS3Connection(ctx context.Context) error {
	cfg, err := s.EffectiveConfig()
	if err != nil {
		return fmt.Errorf("%w: %v", storage.ErrS3ProbeUnavailable, err)
	}
	if cfg.Backend != "s3" {
		return storage.ErrS3ProbeUnavailable
	}
	if err := Validate(cfg); err != nil {
		return fmt.Errorf("%w: %v", storage.ErrS3ProbeUnavailable, err)
	}
	prov, err := NewProviderForConfig(cfg, "_probe", nil, nil)
	if err != nil {
		// A client that cannot be built says the config is wrong, not that the
		// connection failed. Reporting it as a verdict would clear a real
		// outage the moment somebody mistyped an endpoint.
		return fmt.Errorf("%w: %v", storage.ErrS3ProbeUnavailable, err)
	}
	_, err = prov.ListFiles(ctx, "")
	return err
}

// BuildProvider returns a provider SCOPED to subPrefix, built
// strictly from the persisted Core file storage config. There is no
// legacy-directory fallback: the owner has confirmed there are no pre-
// existing DYLARIS installs anywhere, so every install starts with no local
// data to fall back to - a missing/invalid config is therefore a hard error,
// and callers must never proceed with a nil provider.
//
// A provider that still fails to CONSTRUCT despite a valid-looking config
// (e.g. a bad S3 endpoint rejected at client-construction time) is logged
// here, once, so that failure is never silent - callers only ever surface a
// generic "not configured" style response to the client either way.
func (s *Manager) BuildProvider(subPrefix string) (storage.StorageProvider, error) {
	cfg, err := s.EffectiveConfig()
	if err != nil {
		return nil, err
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	prov, err := NewProviderForConfig(cfg, subPrefix, s.Gate, s.S3)
	if err != nil {
		// An unreachable host path is not logged here. It is a per-REQUEST
		// build, so a wedged mount would write one line per request for as
		// long as the outage lasts, drowning the log at exactly the moment it
		// is being read. The gate's own transition is the signal for that
		// case; this line stays for the failures nothing else reports.
		if !errors.Is(err, storage.ErrBackendUnreachable) {
			log.Printf("core storage: failed to build %s provider: %v", subPrefix, err)
		}
		return nil, err
	}
	return prov, nil
}

// NewProviderForConfig builds a StorageProvider strictly from cfg
// (assumed already validated), scoped to subPrefix. Used both by
// BuildProvider (which loads the persisted config first) and by
// TestConnection (which validates a candidate config that may not be saved
// yet), so provider construction lives in exactly one place either way.
//
// gate is the watchdog for the LIVE configured host path, and it is passed in
// rather than read from Manager because not every caller is building against
// that path: the migration target and the connection test both hand this
// function a candidate config the gate knows nothing about. Those pass nil,
// which disables gating for that build. The gate never applies to s3.
//
// s3res is the same arrangement for the s3 backend, and the nil cases matter
// for a second reason there. The connection probe MUST fail fast: pausing a
// "test connection" click for up to ten minutes to re-confirm an endpoint the
// admin just typed wrong would be indefensible. The migration target passes nil
// because, like the gate, the live instance knows nothing about a candidate
// config. s3res never applies to a host path.
func NewProviderForConfig(cfg Config, subPrefix string, gate *storage.Gate, s3res *storage.S3Resilience) (storage.StorageProvider, error) {
	if cfg.Backend == "s3" {
		prefix := subPrefix
		if cfg.S3Prefix != "" {
			prefix = cfg.S3Prefix + "/" + subPrefix
		}
		prov, err := storage.NewProvider("s3", "", map[string]string{
			storage.OptS3Endpoint:  cfg.S3Endpoint,
			storage.OptS3Bucket:    cfg.S3Bucket,
			storage.OptS3Region:    cfg.S3Region,
			storage.OptS3AccessKey: cfg.S3AccessKey,
			storage.OptS3SecretKey: cfg.S3SecretKey,
			storage.OptS3PathStyle: strconv.FormatBool(cfg.S3PathStyle),
			storage.OptS3Prefix:    prefix,
		})
		if err != nil {
			return nil, err
		}
		if s3res == nil {
			return prov, nil
		}
		return storage.NewS3ResilientProvider(prov, s3res), nil
	}
	root := filepath.Join(cfg.Path, subPrefix)
	// BEFORE the MkdirAll, and that ordering is the whole point. This function
	// runs on every provider build and providers are built per request, so on
	// a mount that has stopped answering this MkdirAll is itself the call that
	// blocks - before any provider method is ever reached. Checking after it
	// would gate nothing that matters.
	if err := storage.GatedProviderBlocked(gate); err != nil {
		return nil, err
	}
	// Under the same concurrency bound as every provider method, for the same
	// reason: on a wedged mount this MkdirAll is a syscall that never returns,
	// and one per request is unbounded. MkdirAll's OWN error stays ignored as
	// before; only the gate giving up on it aborts the build.
	//
	// The wait for a SLOT is bounded here, which it was not before. gate.Do's
	// context bounds the acquire (the deadline the gate arms only starts once a
	// slot is held), so passing context.Background() meant a build could wait
	// for a free slot forever - and, having no request context, could not be
	// cancelled when the client hung up or Core shut down. A genuinely wedged
	// mount is already caught fast by GatedProviderBlocked above; what remains
	// is legitimate saturation (every slot held by an in-flight download), which
	// is healthy backpressure but must still be finite. If a slot does not free
	// within the deadline the build fails with a retryable error instead of
	// pinning a goroutine that shutdown cannot drain.
	//
	// Threading the real request context through would react to a client
	// disconnect immediately, but BuildProvider is a
	// func(subPrefix)-shaped closure at ~39 call sites and the modpack factory;
	// the bound removes the unbounded-hang without that churn.
	acquireCtx, cancelAcquire := context.WithTimeout(context.Background(), storageSlotWaitDeadline)
	defer cancelAcquire()
	if err := gate.Do(acquireCtx, func() error {
		_ = os.MkdirAll(root, 0755)
		return nil
	}); err != nil {
		return nil, err
	}
	prov, err := storage.NewProvider("path", root, nil)
	if err != nil {
		return nil, err
	}
	return storage.NewGatedProvider(prov, gate), nil
}

// --- Admin HTTP surface: config CRUD + test-connection ---

// NormalizeCandidate trims whitespace from every free-text field
// of a submitted candidate, so a clipboard-pasted value (path, or any s3
// field) with leading/trailing whitespace doesn't silently persist as-is and
// break the backend or auth later. SaveConfig and TestConnection both route
// their candidate through this single helper so they cannot drift again.
func NormalizeCandidate(c Config) Config {
	c.Backend = strings.TrimSpace(c.Backend)
	c.Path = strings.TrimSpace(c.Path)
	c.S3Endpoint = strings.TrimSpace(c.S3Endpoint)
	c.S3Bucket = strings.TrimSpace(c.S3Bucket)
	c.S3AccessKey = strings.TrimSpace(c.S3AccessKey)
	c.S3SecretKey = strings.TrimSpace(c.S3SecretKey)
	return c
}

// MergeCandidate resolves the effective config to act on: an
// empty request-body backend means "nothing submitted", so the caller falls
// back to the stored config entirely.
//
// Otherwise, a blank secret in the candidate reuses the stored secret ONLY
// when the identity-defining fields (S3Endpoint, S3Bucket, S3AccessKey) all
// match the stored config; the caller is expected to have already run the
// candidate through NormalizeCandidate so this comparison isn't
// fooled by whitespace. If any identity field differs, the secret is left
// blank so Validate rejects the request and the admin must
// re-enter the secret. This matters for two reasons:
//   - Security: settings.write is a delegatable panel capability, not
//     owner-only, and GET always blanks the stored secret. Without this
//     check, a holder of settings.write who cannot read the secret could
//     redirect it to an endpoint/bucket/access-key of their choosing merely
//     by submitting those fields with a blank secret - a credential-
//     rebinding gap (SigV4 signs with the secret, it never goes on the
//     wire, so this is not a plaintext leak, but it lets an attacker's
//     chosen host receive a validly-signed request).
//   - Usability: changing just the access key while leaving the secret
//     blank (because GET hides it) would otherwise silently persist a NEW
//     access key paired with the OLD secret, producing a broken config with
//     no error.
func MergeCandidate(candidate, existing Config) Config {
	if candidate.Backend == "" {
		return existing
	}
	identityUnchanged := candidate.S3Endpoint == existing.S3Endpoint &&
		candidate.S3Bucket == existing.S3Bucket &&
		candidate.S3AccessKey == existing.S3AccessKey
	if candidate.S3SecretKey == "" && identityUnchanged {
		candidate.S3SecretKey = existing.S3SecretKey
	}
	return candidate
}

// coreStorageProbePayload is the fixed content the connection-test probe
// writes and expects back verbatim.
const coreStorageProbePayload = "dylaris-probe"

// ProbeProvider writes, reads back and deletes a uniquely-named probe
// object to verify the backend is reachable and read/write-consistent. The
// probe key is deleted on every path, including a write error, a read error,
// or a read-back/content mismatch, so a broken candidate config never leaves
// a stray object behind.
func ProbeProvider(ctx context.Context, prov storage.StorageProvider) (ok bool, message string) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	key := "probe-" + hex.EncodeToString(b) + ".txt"

	if err := prov.WriteFile(ctx, key, strings.NewReader(coreStorageProbePayload)); err != nil {
		// LocalProvider no longer leaves a partial object behind on a failed
		// write (it stages and renames), but the interface does not promise
		// that of a backend, and DeletePath is idempotent. Clean up on this
		// path too rather than trusting every implementation to be tidy.
		_ = prov.DeletePath(ctx, key)
		return false, "Write failed: " + err.Error()
	}
	rc, err := prov.GetFile(ctx, key)
	if err != nil {
		_ = prov.DeletePath(ctx, key)
		return false, "Read-back failed: " + err.Error()
	}
	got, readErr := io.ReadAll(rc)
	rc.Close()
	if readErr != nil {
		_ = prov.DeletePath(ctx, key)
		return false, "Read-back failed: " + readErr.Error()
	}
	if string(got) != coreStorageProbePayload {
		_ = prov.DeletePath(ctx, key)
		return false, "Read-back mismatch: storage backend is not consistent"
	}
	if err := prov.DeletePath(ctx, key); err != nil {
		return false, "Cleanup failed: " + err.Error()
	}
	return true, "Storage reachable: write, read and delete all succeeded."
}
