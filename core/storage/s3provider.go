package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/smithy-go"

	"dylaris-core/storage/backup"
)

// objectStore is the S3-shaped seam S3Provider needs. *backup.S3Storage
// satisfies it; tests supply an in-memory fake.
type objectStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]backup.Object, error)
	DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error)
	UploadURL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// S3Provider implements StorageProvider against an S3-compatible object store.
// Every key is namespaced under `prefix` so one bucket can host the library,
// ticket-attachments and ticket-backups subsystems side by side.
type S3Provider struct {
	os     objectStore
	prefix string
}

// newS3ProviderFromOpts builds an S3Provider by marshalling the opt map into a
// backup.S3Config and reusing the existing backup.S3Storage client.
func newS3ProviderFromOpts(opts map[string]string) (StorageProvider, error) {
	cfg := backup.S3Config{
		Endpoint:        opts[OptS3Endpoint],
		Region:          opts[OptS3Region],
		Bucket:          opts[OptS3Bucket],
		AccessKeyID:     opts[OptS3AccessKey],
		SecretAccessKey: opts[OptS3SecretKey],
		ForcePathStyle:  opts[OptS3PathStyle] == "true",
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	client, err := backup.NewS3(context.Background(), raw)
	if err != nil {
		return nil, err
	}
	return &S3Provider{os: client, prefix: strings.Trim(opts[OptS3Prefix], "/")}, nil
}

// key maps a provider-relative path to a full object key (prefix + clean path).
// Uses "path" (always forward-slash, no OS/UNC semantics), not "path/filepath":
// S3 keys are never local filesystem paths, and filepath.Clean on Windows
// parses a leading "//" as a UNC prefix instead of collapsing it.
func (p *S3Provider) key(reqPath string) string {
	clean := strings.TrimPrefix(path.Clean("/"+reqPath), "/")
	if p.prefix == "" {
		return clean
	}
	if clean == "" {
		return p.prefix
	}
	return p.prefix + "/" + clean
}

// listPrefix returns the object prefix used to enumerate one directory level.
func (p *S3Provider) listPrefix(reqPath string) string {
	k := p.key(reqPath)
	if k == "" {
		return ""
	}
	return k + "/"
}

// pathObjects returns exactly the objects that belong to reqPath: the object
// at that exact key (if any), plus everything nested under it as a directory.
// It deliberately excludes siblings whose key merely starts with the same
// string - a bare-prefix List(ctx, p.key(reqPath)) would otherwise also match
// e.g. "world_nether/level.dat" when reqPath is "world", since S3
// ListObjectsV2 prefix matching (and the fake) is plain string-prefix
// matching, not directory-boundary-aware.
func (p *S3Provider) pathObjects(ctx context.Context, reqPath string) ([]backup.Object, error) {
	k := p.key(reqPath)
	dirPfx := p.listPrefix(reqPath)
	all, err := p.os.List(ctx, k)
	if err != nil {
		return nil, err
	}
	out := make([]backup.Object, 0, len(all))
	for _, o := range all {
		if o.Key == k || strings.HasPrefix(o.Key, dirPfx) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (p *S3Provider) WriteFile(ctx context.Context, path string, content io.Reader) error {
	return p.os.Put(ctx, p.key(path), content, 0)
}

// GetFile returns a reader for path, normalizing a recognised "object does
// not exist" error to be errors.Is(err, fs.ErrNotExist)-comparable. Callers
// (notably the storage migration's skip-if-exists probe,
// services/storagemigrate/copy.go:targetAlreadyMatches) need to tell
// "genuinely missing" apart from any other backend failure
// (503/timeout/throttle/permission), which on S3 can fail a GetObject just as
// easily as a missing key - collapsing both into "missing" would make a
// transient error look like "safe to overwrite". Any other error is returned
// unwrapped.
func (p *S3Provider) GetFile(ctx context.Context, path string) (io.ReadCloser, error) {
	rc, err := p.os.Get(ctx, p.key(path))
	if err != nil {
		if isS3NotFound(err) {
			return nil, fmt.Errorf("s3 GetFile %s: %w", path, fs.ErrNotExist)
		}
		return nil, err
	}
	return rc, nil
}

// isS3NotFound reports whether an AWS API error indicates a missing object.
// GetObject/DeleteObject return "NoSuchKey"; HeadObject returns "NotFound".
// Mirrors storage/modpack/s3.go's isS3NotFound (same discrimination; not
// shared across packages to keep this seam self-contained).
func isS3NotFound(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code := ae.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound"
	}
	return false
}

func (p *S3Provider) DeletePath(ctx context.Context, reqPath string) error {
	if addressesRoot(reqPath) {
		return ErrDeleteRoot
	}
	// Delete the object at the key AND every object under it (dir semantics),
	// never a sibling whose name merely starts with the same string.
	objs, err := p.pathObjects(ctx, reqPath)
	if err != nil {
		return err
	}
	if len(objs) == 0 {
		return p.os.Delete(ctx, p.key(reqPath))
	}
	for _, o := range objs {
		if err := p.os.Delete(ctx, o.Key); err != nil {
			return err
		}
	}
	return nil
}

// CreateDir is a no-op: object stores have no directories.
func (p *S3Provider) CreateDir(ctx context.Context, path string) error { return nil }

func (p *S3Provider) DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return p.os.DownloadURL(ctx, p.key(key), ttl)
}

// UploadURL presigns a PUT to the same namespaced key DownloadURL reads from,
// so a node writes exactly where Core later lists, stats and deletes. p.key is
// what makes that true: the prefix a storage connection carries lives only on
// this side, and the URL is the one thing that can transport it - the node's
// own s3 config has no prefix field at all.
func (p *S3Provider) UploadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return p.os.UploadURL(ctx, p.key(key), ttl)
}

// ListFiles synthesizes one directory level from the flat key space: files are
// keys with no further "/" after the list prefix; dirs are the distinct first
// path segments of deeper keys.
func (p *S3Provider) ListFiles(ctx context.Context, path string) ([]FileInfo, error) {
	pfx := p.listPrefix(path)
	objs, err := p.os.List(ctx, pfx)
	if err != nil {
		return nil, err
	}
	seenDir := map[string]bool{}
	out := []FileInfo{}
	for _, o := range objs {
		rest := strings.TrimPrefix(o.Key, pfx)
		if rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			name := rest[:i]
			if !seenDir[name] {
				seenDir[name] = true
				out = append(out, FileInfo{Name: name, IsDir: true, Enabled: true})
			}
			continue
		}
		out = append(out, FileInfo{Name: rest, IsDir: false, Size: o.Size, Enabled: true})
	}
	return out, nil
}

// CopyToLocal stages the object(s) into destPath. A .zip source is downloaded
// to a temp file and extracted; anything else is copied verbatim.
func (p *S3Provider) CopyToLocal(ctx context.Context, srcPath, destPath string) error {
	if strings.HasSuffix(strings.ToLower(srcPath), ".zip") {
		rc, err := p.os.Get(ctx, p.key(srcPath))
		if err != nil {
			return err
		}
		defer rc.Close()
		tmp, err := os.CreateTemp("", "dylaris-s3-*.zip")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := io.Copy(tmp, newCtxReader(ctx, rc)); err != nil {
			tmp.Close()
			return err
		}
		tmp.Close()
		return extractZip(ctx, tmp.Name(), destPath)
	}
	// Non-zip: copy every object at the exact key or nested under it into
	// destPath, preserving the relative layout, never a sibling whose name
	// merely starts with the same string.
	objs, err := p.pathObjects(ctx, srcPath)
	if err != nil {
		return err
	}
	if len(objs) == 0 {
		return fmt.Errorf("s3 CopyToLocal: no object at %q", srcPath)
	}
	base := p.key(srcPath)
	if err := os.MkdirAll(destPath, 0755); err != nil {
		return err
	}
	for _, o := range objs {
		rel := strings.TrimPrefix(strings.TrimPrefix(o.Key, base), "/")
		if rel == "" {
			rel = path.Base(o.Key)
		}
		dst := filepath.Join(destPath, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		rc, err := p.os.Get(ctx, o.Key)
		if err != nil {
			return err
		}
		f, err := os.Create(dst)
		if err != nil {
			rc.Close()
			return err
		}
		_, cerr := io.Copy(f, newCtxReader(ctx, rc))
		f.Close()
		rc.Close()
		if cerr != nil {
			return cerr
		}
	}
	return nil
}
