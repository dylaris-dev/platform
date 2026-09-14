package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalStorage writes backup archives to a directory on the Core's filesystem.
// In production this should be a mount that's either backed up itself or is
// the same NFS share used by other Core replicas.
type LocalStorage struct {
	BasePath string
}

// LocalConfig is the JSON shape of the storage `config` column.
type LocalConfig struct {
	BasePath string `json:"basePath"`
}

func NewLocal(raw json.RawMessage) (*LocalStorage, error) {
	var cfg LocalConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("invalid local config: %w", err)
		}
	}
	if cfg.BasePath == "" {
		return nil, fmt.Errorf("local storage requires basePath")
	}
	abs, err := filepath.Abs(cfg.BasePath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create base path: %w", err)
	}
	return &LocalStorage{BasePath: abs}, nil
}

func (l *LocalStorage) Provider() string { return "local" }

// resolveKey joins key onto BasePath while guarding against `..` traversal.
func (l *LocalStorage) resolveKey(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	full := filepath.Join(l.BasePath, clean)
	if !strings.HasPrefix(full, l.BasePath) {
		return "", fmt.Errorf("invalid key: %s", key)
	}
	return full, nil
}

// backupUploadTempPrefix marks the half-written archives Put leaves behind when
// a transfer dies between the copy and the rename. A fixed, recognisable prefix
// lets List hide them so a partial upload is never served as a real backup, and
// lets orphans be swept.
const backupUploadTempPrefix = ".dylaris-backup-upload-"

// ctxReader aborts a copy between chunks once ctx is done. It cannot unblock a
// read already stuck in a syscall on a wedged mount - nothing in userspace can -
// but it stops a copy that is still making progress (a cancelled job, a dropped
// stream).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Put stages the archive next to its destination and renames it into place, so
// the destination key only ever refers to a complete file.
//
// Writing straight into the destination truncated the real name up front, so a
// transfer that died mid-copy - a dropped stream, a mounted share going away -
// either published a truncated archive under the real name or destroyed the
// previous good one. Silent backup corruption; this is the fix. It mirrors the
// core LocalProvider.WriteFile, which was fixed the same way.
//
// The temp file lives in the destination directory because rename cannot cross
// filesystems. rename is atomic on local POSIX; neither CIFS nor NFS guarantees
// it, so on a mounted share this is a large improvement over truncate-in-place,
// not a guarantee.
func (l *LocalStorage) Put(ctx context.Context, key string, r io.Reader, _ int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := l.resolveKey(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, backupUploadTempPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	// CreateTemp is 0600; archives are read back by Core (and by other replicas
	// when BasePath is a share), so keep the previous 0644 mode.
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if _, err := io.Copy(tmp, &ctxReader{ctx: ctx, r: r}); err != nil {
		return err
	}
	// Sync before rename so a network filesystem that buffers the write and
	// fails on flush surfaces the error here, instead of renaming a file whose
	// tail never landed.
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, full); err != nil {
		return err
	}
	committed = true
	return nil
}

func (l *LocalStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	full, err := l.resolveKey(key)
	if err != nil {
		return nil, err
	}
	return os.Open(full)
}

func (l *LocalStorage) Delete(_ context.Context, key string) error {
	full, err := l.resolveKey(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (l *LocalStorage) List(_ context.Context, prefix string) ([]Object, error) {
	root, err := l.resolveKey(prefix)
	if err != nil {
		return nil, err
	}
	var out []Object
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		// Skip staging temp files Put may have left behind (see Put): a partial
		// upload must never be listed or served as a real backup archive.
		if strings.HasPrefix(filepath.Base(path), backupUploadTempPrefix) {
			return nil
		}
		rel, _ := filepath.Rel(l.BasePath, path)
		out = append(out, Object{
			Key:          filepath.ToSlash(rel),
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (l *LocalStorage) Stat(_ context.Context, key string) (Object, error) {
	full, err := l.resolveKey(key)
	if err != nil {
		return Object{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return Object{}, err
	}
	return Object{
		Key:          key,
		Size:         info.Size(),
		LastModified: info.ModTime(),
	}, nil
}

func (l *LocalStorage) DownloadURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	// LocalStorage has no pre-signed URL. Caller streams via Core.
	return "", nil
}

func (l *LocalStorage) UploadURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	// LocalStorage has no pre-signed URL.
	return "", nil
}

func (*LocalStorage) CreateMultipart(context.Context, string) (string, error) {
	return "", ErrMultipartUnsupported
}

func (*LocalStorage) UploadPartURL(context.Context, string, string, int32, time.Duration) (string, error) {
	return "", ErrMultipartUnsupported
}

func (*LocalStorage) CompleteMultipart(context.Context, string, string, int64) (int64, error) {
	return 0, ErrMultipartUnsupported
}

func (*LocalStorage) AbortMultipart(context.Context, string, string) error {
	return ErrMultipartUnsupported
}

func (*LocalStorage) ListMultipart(context.Context, string, string) (MultipartUsage, error) {
	return MultipartUsage{}, ErrMultipartUnsupported
}
