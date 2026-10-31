// Package storagemigrate holds the one migration + verification engine used
// by all the blob data sets (ticket-attachments, ticket-backups,
// modpacks, server-backups). Core has three deliberately different storage
// interfaces (storage.StorageProvider, storage/backup.Storage,
// storage/modpack.ModpackStorageProvider); rather than write 3xN code paths,
// each data set adapts to the ONE narrow DataSet seam below exactly once.
package storagemigrate

import (
	"context"
	"io"
)

// ObjectRef is one enumerable object in a data set. Deliberately backend
// agnostic: no mtime (StorageProvider cannot cheaply produce one), no ETag
// (a multipart ETag is not a content hash), no storage class.
type ObjectRef struct {
	Key  string
	Size int64
}

// DataSet is the uniform read/write surface the migration engine needs.
// Deliberately narrow: no Stat, no presign, no directory semantics. Every
// implementation is a thin adapter over one of the three existing storage
// interfaces.
type DataSet interface {
	// ID is the stable identifier, e.g. "ticket-attachments" or "server-backups:3".
	ID() string
	// Label is the human name shown in the panel.
	Label() string
	// List enumerates the ENTIRE key space, recursively, sorted ascending
	// by Key. An empty data set returns an empty slice, not an error.
	List(ctx context.Context) ([]ObjectRef, error)
	// Open returns a reader for key. A genuinely missing key MUST return an
	// error for which errors.Is(err, fs.ErrNotExist) is true, and every
	// OTHER failure (throttle, 503, permission) MUST NOT satisfy that - the
	// copy loop treats "missing" as "copy it" and anything else as a hard
	// failure, so collapsing the two would silently overwrite live data.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Write stores key. size may be -1 when unknown.
	Write(ctx context.Context, key string, r io.Reader, size int64) error
	// Delete removes key. Idempotent: deleting an absent key is not an error.
	Delete(ctx context.Context, key string) error
}
