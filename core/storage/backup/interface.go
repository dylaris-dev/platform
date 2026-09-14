package backup

import (
	"context"
	"errors"
	"io"
	"time"
)

// Object is a single artifact stored in a BackupStorage.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Storage is the interface that any backup backend must satisfy.
// Implementations are kept narrow — the worker streams a single tar.gz per
// run, the panel/handler streams it back out, and the cron job lists keys
// for retention. Multipart exists only so Core can drive a node's upload
// through presigned part URLs; checksums and encryption are not here.
type Storage interface {
	// Provider returns the textual provider name, e.g. "local" or "s3".
	Provider() string

	// Put stores the byte stream under `key`. Caller is responsible for
	// closing `r` after Put returns.
	Put(ctx context.Context, key string, r io.Reader, size int64) error

	// Get returns a reader for `key`. Caller MUST close.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Delete removes the object under `key`. Returns nil even when the key
	// did not exist (so callers can be idempotent).
	Delete(ctx context.Context, key string) error

	// List returns all objects under `prefix`. Empty slice when nothing
	// matches (not an error).
	List(ctx context.Context, prefix string) ([]Object, error)

	// Stat returns metadata for `key`. A missing key is reported so that
	// errors.Is(err, fs.ErrNotExist) is true; any other error means the
	// backend could not answer, which is NOT evidence the key is absent.
	// Callers that act on absence must tell the two apart - the backup reaper
	// decides from this whether a run wrote an archive before it went silent.
	//
	// All FOUR implementations are held to this, and the count is worth
	// stating because an earlier version of this comment said "both" and was
	// wrong twice over. LocalStorage inherits it from os.Stat;
	// CoreStorageBackupAdapter wraps its own miss; S3Storage wraps the SDK's
	// NotFound; NodeLocalStorage wraps its not-found error. The last two did
	// not always do so, and the reaper acting on this contract is what made
	// each of them observable.
	Stat(ctx context.Context, key string) (Object, error)

	// DownloadURL returns a pre-signed GET URL valid for the given duration if
	// the provider supports it. LocalStorage returns ("", nil) — the panel
	// falls back to streaming via Core in that case.
	DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error)

	// UploadURL returns a pre-signed PUT URL valid for the given duration if the
	// provider supports it (S3/R2). Used to let a BYON tenant node upload a
	// backup WITHOUT ever receiving the bucket credentials. Non-S3 providers
	// return ("", nil).
	UploadURL(ctx context.Context, key string, ttl time.Duration) (string, error)

	// The four multipart operations below let Core own an upload whose parts a
	// node sends through presigned URLs, so the node never holds the bucket
	// credentials and the single-PUT 5 GiB ceiling of UploadURL does not apply.
	// Backends without object storage return ErrMultipartUnsupported from all
	// four. Keys are namespaced exactly as UploadURL namespaces them, so an
	// object completed here is the one Stat, Get, Delete and retention read.

	// CreateMultipart starts a multipart upload for key and returns its upload id.
	CreateMultipart(ctx context.Context, key string) (uploadID string, err error)

	// UploadPartURL presigns one UploadPart (partNumber 1..10000) of that upload.
	UploadPartURL(ctx context.Context, key, uploadID string, partNumber int32, ttl time.Duration) (string, error)

	// CompleteMultipart lists the uploaded parts itself and never trusts ETags
	// reported by the uploader. It refuses unless the parts are numbered 1..N
	// without gaps and every part but the last is exactly partSize bytes (the
	// last may be smaller, never larger), then completes the upload and returns
	// the object's total size.
	CompleteMultipart(ctx context.Context, key, uploadID string, partSize int64) (int64, error)

	// AbortMultipart aborts the upload. An upload that no longer exists is not
	// an error.
	AbortMultipart(ctx context.Context, key, uploadID string) error
}

// ErrMultipartUnsupported is returned by every multipart operation of a
// backend that has no object storage behind it (a filesystem path, a node's
// local disk).
var ErrMultipartUnsupported = errors.New("backup storage: multipart upload not supported for this backend")

// ErrUploadURLUnsupported is returned by backends that cannot presign an
// upload. Callers MUST already have gated on Provider()=="s3" before asking:
// services/node_storage_access.go:51 and services/migration_orchestrator.go:658
// both do. LocalStorage and NodeLocalStorage keep their existing ("", nil)
// returns - that IS their documented contract and changing it would alter
// behavior outside this workstream's scope.
var ErrUploadURLUnsupported = errors.New("backup storage: presigned upload not supported for this backend")
