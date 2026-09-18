package modpack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"time"

	"dylaris-core/storage"
)

// CoreStorageSubPrefix is the namespace modpack objects occupy inside the ONE
// configured Core file storage. It duplicates the canonical
// handlers.CoreStoragePrefixModpacks because this package must not import
// handlers (handlers already imports this package). The two are held together
// by handlers.TestCoreStorageSubPrefixesMatch.
const CoreStorageSubPrefix = "modpacks"

// CoreStorageProvider satisfies ModpackStorageProvider by delegating to the
// shared Core file storage provider, scoped to CoreStorageSubPrefix. A thin
// adapter, not a new backend: whatever the Core file storage is configured as
// (path or s3) is what .mrpack objects land on.
//
// It takes a storage.StorageProvider directly, unlike the backup side which
// takes an already-adapted backup.Storage. That asymmetry is not an
// oversight: package `storage` already imports `storage/backup`, so
// storage/backup cannot import `storage` back, whereas this package has no
// such constraint.
//
// On buffering: the []byte contract is fully satisfiable with no regression.
// The existing S3 modpack provider already does io.ReadAll on Get
// (s3.go:81) and bytes.NewReader on Put (s3.go:59), so this adapter has a
// byte-for-byte identical memory profile. Reshaping ModpackStorageProvider to
// stream would touch packs_mrpack.go's zip assembly and the SHA1/MD5
// computation in packs_import.go, and is explicitly out of scope here.
type CoreStorageProvider struct {
	prov storage.StorageProvider
}

// NewCoreStorageProvider wraps a scoped Core file storage provider.
func NewCoreStorageProvider(p storage.StorageProvider) *CoreStorageProvider {
	return &CoreStorageProvider{prov: p}
}

func (p *CoreStorageProvider) Put(ctx context.Context, key string, data []byte) error {
	if err := p.prov.WriteFile(ctx, key, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("modpack storage: core-storage put %s: %w", key, err)
	}
	return nil
}

// Get reads the whole object. A genuinely missing key is translated to
// ErrNotFound, because every caller in handlers/packs_*.go branches on
// ErrNotFound and would otherwise treat a 404 as a 500.
func (p *CoreStorageProvider) Get(ctx context.Context, key string) ([]byte, error) {
	rc, err := p.prov.GetFile(ctx, key)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("modpack storage: core-storage get %s: %w", key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("modpack storage: core-storage read %s: %w", key, err)
	}
	return data, nil
}

// PutStream delegates to the underlying storage provider's WriteFile, which
// takes an io.Reader and streams it. The size is not forwarded: WriteFile has
// no size parameter, so a core-storage-on-S3 backend may still let the AWS SDK
// determine the length itself. The local backend streams through a temp file
// and rename regardless. This is the one PutStream that is not guaranteed
// end-to-end streaming on S3; the dedicated s3 modpack backend (S3Provider) is.
func (p *CoreStorageProvider) PutStream(ctx context.Context, key string, r io.Reader, _ int64) error {
	if err := p.prov.WriteFile(ctx, key, r); err != nil {
		return fmt.Errorf("modpack storage: core-storage put-stream %s: %w", key, err)
	}
	return nil
}

// Stream delegates to the underlying storage provider's GetFile, which already
// returns a stream on both backends - the buffering this replaces was added by
// this adapter's own Get, not by the layer below it.
//
// It always reports SizeUnknown, and that is a correctness requirement, not a
// shortcut.
//
// StorageProvider has no size on GetFile and no Stat, so the only way to learn
// one here is Stat, which lists the key's parent directory. On the gated host
// path backend that is a second trip through the shared 128-slot filesystem
// semaphore - while the reader GetFile just returned is STILL HOLDING a slot of
// its own, because that slot is only released on Close, out in the handler.
// Asking for the size therefore means holding one slot and queueing for
// another. With enough concurrent streams every slot is held by a reader whose
// owner is blocked waiting for a slot that can never free, and since acquireFS
// deliberately does not fail fast, the whole filesystem side of Core wedges
// permanently against a perfectly healthy backend. The public pack mirror is
// one of the callers, so it does not take an authenticated user to get there.
//
// Doing the Stat FIRST and the open second would avoid the nesting, but it
// would describe a different object than the one being served if the key were
// rewritten in between, and a wrong Content-Length leaves the client hanging or
// truncating. Omitting the header costs a chunked response and nothing else.
//
// LocalProvider.Stream still reports a real size: it takes it from the open
// file handle, so it needs no second acquisition and has no such window.
func (p *CoreStorageProvider) Stream(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	rc, err := p.prov.GetFile(ctx, key)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("modpack storage: core-storage stream %s: %w", key, err)
	}
	return rc, SizeUnknown, nil
}

// DownloadURL delegates to the Core file storage provider, so a modpack kept
// on a core storage backed by S3 becomes a redirect exactly like a library
// download already is. The path backend returns ("", nil) and the caller
// streams.
//
// This is the case that made the public mirror route hold whole packs in the
// heap: routing modpacks through core storage meant they could never be
// presigned, because this adapter had no way to express one.
func (p *CoreStorageProvider) DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	url, err := p.prov.DownloadURL(ctx, key, ttl)
	if err != nil {
		return "", fmt.Errorf("modpack storage: core-storage presign %s: %w", key, err)
	}
	return url, nil
}

// Delete is idempotent: LocalProvider.DeletePath is os.RemoveAll (nil on
// missing) and S3Provider.DeletePath returns nil for a missing key.
func (p *CoreStorageProvider) Delete(ctx context.Context, key string) error {
	if err := p.prov.DeletePath(ctx, key); err != nil {
		return fmt.Errorf("modpack storage: core-storage delete %s: %w", key, err)
	}
	return nil
}

// Stat reports (size, exists, err) by listing the key's PARENT directory,
// exactly like the backup adapter. StorageProvider has no Stat, and a GetFile
// probe would issue a full GetObject on S3 just to learn a size.
func (p *CoreStorageProvider) Stat(ctx context.Context, key string) (int64, bool, error) {
	dir := path.Dir(key)
	if dir == "." || dir == "/" {
		dir = ""
	}
	entries, err := p.prov.ListFiles(ctx, dir)
	if err != nil {
		return 0, false, fmt.Errorf("modpack storage: core-storage stat %s: %w", key, err)
	}
	base := path.Base(key)
	for _, e := range entries {
		if e.IsDir || e.Name != base {
			continue
		}
		return e.Size, true, nil
	}
	return 0, false, nil
}
