package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/storage/backup"
)

var multipartOps = []string{"CreateMultipart", "UploadPartURL", "CompleteMultipart", "AbortMultipart", "ListMultipart"}

// callMultipart runs one multipart operation, by name, through p.
func callMultipart(p StorageProvider, op, key string) error {
	ctx := context.Background()
	switch op {
	case "CreateMultipart":
		_, err := p.CreateMultipart(ctx, key)
		return err
	case "UploadPartURL":
		_, err := p.UploadPartURL(ctx, key, "upload-1", 1, time.Minute)
		return err
	case "CompleteMultipart":
		_, err := p.CompleteMultipart(ctx, key, "upload-1", 5<<20)
		return err
	case "AbortMultipart":
		return p.AbortMultipart(ctx, key, "upload-1")
	case "ListMultipart":
		_, err := p.ListMultipart(ctx, key, "upload-1")
		return err
	}
	panic("unknown multipart op " + op)
}

// S3Provider must hand every multipart operation the namespaced key, or a node
// assembles its archive somewhere GetFile, ListFiles and DeletePath never look.
func TestS3Provider_Multipart_AppliesPrefix(t *testing.T) {
	fos := newFakeObjectStore()
	p := &S3Provider{os: fos, prefix: "server-backups"}

	for _, op := range multipartOps {
		if err := callMultipart(p, op, "backups/srv-1/a.tar.gz"); err != nil {
			t.Errorf("%s: %v", op, err)
		}
		if got, want := fos.multipartKeys[op], "server-backups/backups/srv-1/a.tar.gz"; got != want {
			t.Errorf("%s reached the object store with key %q, want %q", op, got, want)
		}
	}
	url, _ := p.UploadPartURL(context.Background(), "a.tar.gz", "upload-1", 3, time.Minute)
	if want := "https://signed-part.example/server-backups/a.tar.gz?partNumber=3&uploadId=upload-1"; url != want {
		t.Errorf("UploadPartURL = %q, want %q", url, want)
	}
}

// The adapter is a pass-through for multipart like for everything else: the
// s3 provider below answers with the prefixed key, the path provider refuses.
func TestCoreStorageBackupAdapter_MultipartPassesThrough(t *testing.T) {
	t.Run("s3 backend", func(t *testing.T) {
		fos := newFakeObjectStore()
		a := NewCoreStorageBackupAdapter(&S3Provider{os: fos, prefix: "server-backups"})
		ctx := context.Background()
		if id, err := a.CreateMultipart(ctx, "srv-1/a.tar.gz"); err != nil || id != "upload-1" {
			t.Errorf("CreateMultipart = (%q, %v), want (upload-1, nil)", id, err)
		}
		if url, err := a.UploadPartURL(ctx, "srv-1/a.tar.gz", "upload-1", 1, time.Minute); err != nil || url == "" {
			t.Errorf("UploadPartURL = (%q, %v), want a URL", url, err)
		}
		if size, err := a.CompleteMultipart(ctx, "srv-1/a.tar.gz", "upload-1", 5<<20); err != nil || size != 42 {
			t.Errorf("CompleteMultipart = (%d, %v), want (42, nil)", size, err)
		}
		if err := a.AbortMultipart(ctx, "srv-1/a.tar.gz", "upload-1"); err != nil {
			t.Errorf("AbortMultipart = %v, want nil", err)
		}
		if _, err := a.ListMultipart(ctx, "srv-1/a.tar.gz", "upload-1"); err != nil {
			t.Errorf("ListMultipart = %v, want nil", err)
		}
		for _, op := range multipartOps {
			if fos.attempts[op] != 1 {
				t.Errorf("%s reached the object store %d times, want 1", op, fos.attempts[op])
			}
			if got := fos.multipartKeys[op]; got != "server-backups/srv-1/a.tar.gz" {
				t.Errorf("%s key = %q, want the prefixed key", op, got)
			}
		}
	})

	t.Run("path backend", func(t *testing.T) {
		a := NewCoreStorageBackupAdapter(&LocalProvider{BasePath: t.TempDir()})
		ctx := context.Background()
		if _, err := a.CreateMultipart(ctx, "k"); !errors.Is(err, backup.ErrMultipartUnsupported) {
			t.Errorf("CreateMultipart = %v, want ErrMultipartUnsupported", err)
		}
		if _, err := a.UploadPartURL(ctx, "k", "u", 1, time.Minute); !errors.Is(err, backup.ErrMultipartUnsupported) {
			t.Errorf("UploadPartURL = %v, want ErrMultipartUnsupported", err)
		}
		if _, err := a.CompleteMultipart(ctx, "k", "u", 5<<20); !errors.Is(err, backup.ErrMultipartUnsupported) {
			t.Errorf("CompleteMultipart = %v, want ErrMultipartUnsupported", err)
		}
		if err := a.AbortMultipart(ctx, "k", "u"); !errors.Is(err, backup.ErrMultipartUnsupported) {
			t.Errorf("AbortMultipart = %v, want ErrMultipartUnsupported", err)
		}
	})
}

// Pins the retry decision per operation across a transport blip that a second
// attempt would survive. Presigning and aborting replay safely and must retry;
// Create could open a second upload and Complete could fail an upload it
// already completed, so each must reach the store exactly once.
func TestS3Resilience_MultipartRetryDecisions(t *testing.T) {
	tests := []struct {
		op        string
		wantRetry bool
	}{
		{"UploadPartURL", true},
		{"AbortMultipart", true},
		{"ListMultipart", true},
		{"CreateMultipart", false},
		{"CompleteMultipart", false},
	}
	for _, tt := range tests {
		t.Run(tt.op, func(t *testing.T) {
			fos := newFakeObjectStore()
			fos.failErr, fos.failLeft = connErr(), 1
			res, _ := newTestRes(time.Millisecond, time.Minute)
			p := NewS3ResilientProvider(&S3Provider{os: fos, prefix: "server-backups"}, res)

			err := callMultipart(p, tt.op, "a.tar.gz")

			wantAttempts := 1
			if tt.wantRetry {
				wantAttempts = 2
			}
			if fos.attempts[tt.op] != wantAttempts {
				t.Errorf("%s attempts = %d, want %d", tt.op, fos.attempts[tt.op], wantAttempts)
			}
			if tt.wantRetry && err != nil {
				t.Errorf("%s = %v, want the retry to hide the blip", tt.op, err)
			}
			if !tt.wantRetry && err == nil {
				t.Errorf("%s = nil, want the connection error returned instead of a retry", tt.op)
			}
		})
	}
}

func TestLocalProvider_MultipartUnsupported(t *testing.T) {
	p := &LocalProvider{BasePath: t.TempDir()}
	for _, op := range multipartOps {
		if err := callMultipart(p, op, "k"); !errors.Is(err, backup.ErrMultipartUnsupported) {
			t.Errorf("%s = %v, want ErrMultipartUnsupported", op, err)
		}
	}
}
