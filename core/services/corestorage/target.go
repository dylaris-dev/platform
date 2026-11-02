package corestorage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"dylaris-core/services"
	"dylaris-core/storage"
)

// This file holds everything that must be true about an AD-HOC target storage
// config before a migration may start, plus the single writer that makes one
// active.
//
// Why an ad-hoc config exists at all: ticket-attachments, ticket-backups and
// modpacks are each backed by ONE settings-configured
// backend. Their saved config describes where they live NOW, so it cannot also
// be the destination - reading the target from it would make source == target.
// The operator therefore names the destination inline (another S3, a
// WebDAV/NFS-mounted path, or back), and the engine builds a provider from it.
// This is not new machinery: CoreStorageHandler.TestConnection already builds a
// working provider from an unsaved candidate config through
// NewProviderForConfig. The migration reuses that exact seam.

// ErrTargetSameLocation is returned when a migration target resolves to the
// same physical location as its source.
var ErrTargetSameLocation = errors.New("core storage: the target resolves to the same location as the source")

// ErrTargetNestedLocation is returned when one side of a migration is stored
// INSIDE the other on a shared root. Kept separate from ErrTargetSameLocation
// because the two fail differently: equality rewrites objects onto themselves,
// while containment rebases keys into the other data set's live namespace,
// where a name collision overwrites a live object.
var ErrTargetNestedLocation = errors.New("core storage: one data set is stored inside the other")

// ConfigFromTarget converts the transport shape (declared in
// services, because services cannot import handlers) into the handlers shape.
// This is the ONLY conversion point between the two, so the field mapping
// cannot drift. The candidate is normalized on the way through, exactly as
// SaveConfig and TestConnection normalize theirs, so a clipboard-pasted value
// with stray whitespace does not become a subtly different location.
func ConfigFromTarget(t services.StorageTargetConfig) Config {
	return NormalizeCandidate(Config{
		Backend:       t.Backend,
		Path:          t.Path,
		PathConfirmed: t.PathConfirmed,
		S3Endpoint:    t.S3Endpoint,
		S3Bucket:      t.S3Bucket,
		S3Region:      t.S3Region,
		S3AccessKey:   t.S3AccessKey,
		S3SecretKey:   t.S3SecretKey,
		S3PathStyle:   t.S3PathStyle,
		S3Prefix:      t.S3Prefix,
	})
}

// effectiveS3Prefix is the prefix the same-location comparison treats as "where
// this data set lives" in a bucket: the configured prefix joined with the data
// set's sub-prefix, trimmed of surrounding slashes.
//
// It is NOT literally what NewProviderForConfig hands the S3 provider,
// and the two do diverge for a configured prefix with a stray slash:
// NewProviderForConfig concatenates cfg.S3Prefix raw, so "prod/" makes
// it write under "prod//modpacks" while this reports "prod/modpacks"
// (NormalizeCandidate does not trim S3Prefix, so that config is
// reachable). Teaching the provider to call this would silently relocate every
// object of any install whose saved prefix ends in a slash, so it deliberately
// does not.
//
// The divergence is safe because it only ever runs in the OVER-refusal
// direction: two configs that resolve here to the same string are the same
// place regardless of slashes, so a real alias is still caught. The worst case
// is refusing a target that is in fact distinct, which an operator can see and
// work around; the reverse, accepting a target that is really the source, is
// what would be destructive, and it cannot happen this way.
func effectiveS3Prefix(cfg Config, subPrefix string) string {
	p := strings.Trim(cfg.S3Prefix, "/")
	if p == "" {
		return subPrefix
	}
	return p + "/" + subPrefix
}

// Root is the path-backend directory this data set lives in.
func Root(cfg Config, subPrefix string) string {
	return filepath.Join(cfg.Path, subPrefix)
}

// SameLocation reports whether src and tgt name the same physical
// place.
//
// The two sub-prefixes are separate on purpose. They are usually identical, but
// a data set whose SOURCE provider adds no sub-directory of its own while its
// migration target does is a real case - see storageSourceLocation in
// storage_migration.go for the one that exists (modpacks on its own local
// backend). Feeding a single sub-prefix to both sides there compared
// "<src>/modpacks" against "<tgt>/modpacks" when the source root was already
// the modpacks directory, which is off by one level in the UNDER-refusal
// direction: it accepted a target that resolved to the source itself.
//
// For a path backend this is os.SameFile - device and inode - and NOT a string
// compare. That distinction is load-bearing: "/mnt/old" and "/mnt/new" are
// routinely the same directory through a symlink or a Docker bind mount, and
// accepting such a target would send the copy loop over every source object,
// rewriting each one onto itself, before verification ever ran. A target
// directory that does not exist yet cannot alias anything, so it is not the
// same location (and must not be an error: a first migration onto a fresh
// mount depends on it).
//
// For s3 there is no inode to consult, so identity is endpoint + bucket +
// effective prefix. Credentials are deliberately NOT part of it: a second key
// pair pointing at the same bucket and prefix is still the same bucket and
// prefix. A different prefix in the same bucket IS a distinct location and is
// allowed.
//
// A path backend and an s3 backend are never the same location.
func SameLocation(src, tgt Config, srcSubPrefix, tgtSubPrefix string) (bool, error) {
	srcS3 := src.Backend == "s3"
	tgtS3 := tgt.Backend == "s3"
	if srcS3 != tgtS3 {
		return false, nil
	}
	if srcS3 {
		// Trim, not TrimSuffix, and on the bucket too: the error that matters
		// here is UNDER-refusal. Judging "https://s3.example.com//" and
		// "https://s3.example.com" to be different locations would accept a
		// target that really is the source, which is exactly the outcome this
		// function exists to prevent.
		return strings.Trim(src.S3Endpoint, "/") == strings.Trim(tgt.S3Endpoint, "/") &&
			strings.Trim(src.S3Bucket, "/") == strings.Trim(tgt.S3Bucket, "/") &&
			effectiveS3Prefix(src, srcSubPrefix) == effectiveS3Prefix(tgt, tgtSubPrefix), nil
	}

	// Two configs naming the exact same root are trivially the same location,
	// whether or not that directory has been created yet (e.g. a brand-new
	// install that has never written to this data set). This is a fast-path
	// exact-match short-circuit, not a replacement for the os.SameFile check
	// below: it can only ever add a TRUE positive for a literally-identical
	// path, and does nothing for the harder case of two DIFFERENT path strings
	// aliasing the same directory through a symlink or a bind mount, which
	// os.SameFile below still has to catch.
	if filepath.Clean(Root(src, srcSubPrefix)) == filepath.Clean(Root(tgt, tgtSubPrefix)) {
		return true, nil
	}

	srcInfo, err := os.Stat(Root(src, srcSubPrefix))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat source root: %w", err)
	}
	tgtInfo, err := os.Stat(Root(tgt, tgtSubPrefix))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat target root: %w", err)
	}
	return os.SameFile(srcInfo, tgtInfo), nil
}

// EnsureDistinctLocation refuses a same-location target. It runs at
// Start, before any object is read: discovering this mid-copy would mean the
// engine had already overwritten source objects with themselves.
func EnsureDistinctLocation(src, tgt Config, srcSubPrefix, tgtSubPrefix string) error {
	same, err := SameLocation(src, tgt, srcSubPrefix, tgtSubPrefix)
	if err != nil {
		return err
	}
	if same {
		return fmt.Errorf("%w: pick a different bucket, prefix or path (note that two paths can be the same directory through a symlink or a bind mount)", ErrTargetSameLocation)
	}
	return nil
}

// BuildTargetProvider validates cfg and builds a provider scoped to
// subPrefix. Validation reuses Validate - the same predicate
// the settings form enforces - rather than a second copy of the rules.
func BuildTargetProvider(cfg Config, subPrefix string) (storage.StorageProvider, error) {
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("target storage config: %w", err)
	}
	// nil gate: cfg is a MIGRATION TARGET, a different location from the live
	// one by definition (EnsureDistinctLocation enforces it), so
	// the live path's watchdog has no verdict to offer about it.
	prov, err := NewProviderForConfig(cfg, subPrefix, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("target storage: %w", err)
	}
	return prov, nil
}

// PersistConfig writes cfg to the settings store. It is the ONE
// writer of these keys: SaveConfig calls it, and so does the migration's
// switching_config phase. Two copies would drift, and a drifted writer is how
// an install ends up with a new access key paired to an old secret.
//
// secret is the NEWLY supplied secret to store, and it is passed as a value
// rather than as a "cfg.S3SecretKey is new" flag on purpose. cfg is routinely a
// MERGED config whose S3SecretKey is the one already stored (that is what
// MergeCandidate produces when a request omits the backend), so a
// flag plus cfg would let a caller mean "write the submitted secret" and get
// "rewrite the stored one over itself" - a rotation that returns success and
// changes nothing. With the value passed explicitly, the only secret this can
// write is the one handed to it.
//
// An empty secret leaves the stored one untouched, except that switching the
// backend away from s3 clears it outright so it does not linger orphaned with
// S3SecretSet still reporting true.
func (s *Manager) PersistConfig(cfg Config, secret string) error {
	// 0 stores "" (no connection selected). Written unconditionally like the
	// other non-secret keys, so switching to an inline target via the migration
	// path clears a previously selected connection.
	connID := ""
	if cfg.ConnectionID != 0 {
		connID = strconv.Itoa(cfg.ConnectionID)
	}
	pairs := []struct{ k, v string }{
		{KeyBackend, cfg.Backend},
		{KeyPath, cfg.Path},
		{KeyPathConfirm, strconv.FormatBool(cfg.PathConfirmed)},
		{KeyS3Endpoint, cfg.S3Endpoint},
		{KeyS3Bucket, cfg.S3Bucket},
		{KeyS3Region, cfg.S3Region},
		{KeyS3AccessKey, cfg.S3AccessKey},
		{KeyS3PathStyle, strconv.FormatBool(cfg.S3PathStyle)},
		{KeyS3Prefix, cfg.S3Prefix},
		{KeyConnectionID, connID},
	}
	if secret != "" {
		pairs = append(pairs, struct{ k, v string }{KeyS3SecretKey, secret})
	} else if cfg.Backend != "s3" {
		pairs = append(pairs, struct{ k, v string }{KeyS3SecretKey, ""})
	}
	for _, p := range pairs {
		if err := s.Store.SetSetting(p.k, p.v); err != nil {
			return &SettingWriteError{Key: p.k, Err: err}
		}
	}
	// The watched root follows the config. Being the one writer is exactly why
	// this belongs here: a second writer would leave the watchdog probing a
	// path the platform no longer uses.
	s.SyncGate()
	return nil
}

// SettingWriteError names the settings key whose write failed while
// still wrapping the underlying store error. The key is carried as a field so
// an HTTP caller can name it without putting the raw store error text (which
// can carry DB internals) in a response body.
type SettingWriteError struct {
	Key string
	Err error
}

func (e *SettingWriteError) Error() string {
	return fmt.Sprintf("save setting %s: %v", e.Key, e.Err)
}

func (e *SettingWriteError) Unwrap() error { return e.Err }
