package modpack

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"dylaris-core/storage"
)

// countingProvider records which StorageProvider methods the adapter reaches.
// Only the two Stream can plausibly touch are given behaviour; the rest exist
// to satisfy the interface and fail the test if they are called.
type countingProvider struct {
	t *testing.T

	getFileCalls   int
	listFilesCalls int
}

func (c *countingProvider) GetFile(context.Context, string) (io.ReadCloser, error) {
	c.getFileCalls++
	return io.NopCloser(strings.NewReader("pack-bytes")), nil
}

func (c *countingProvider) ListFiles(context.Context, string) ([]storage.FileInfo, error) {
	c.listFilesCalls++
	return nil, nil
}

func (c *countingProvider) DeletePath(context.Context, string) error { return nil }
func (c *countingProvider) CreateDir(context.Context, string) error  { return nil }
func (c *countingProvider) CopyToLocal(context.Context, string, string) error {
	c.t.Fatal("CopyToLocal was called")
	return nil
}
func (c *countingProvider) WriteFile(context.Context, string, io.Reader) error { return nil }
func (c *countingProvider) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func (c *countingProvider) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

// TestCoreStorageStreamAcquiresOneSlotOnly is a deadlock guard, not a
// behaviour test.
//
// On the gated host-path backend, the reader GetFile returns holds one of the
// 128 shared filesystem-semaphore slots until it is closed - and it is closed
// out in the handler, long after Stream returns. ListFiles takes a slot of its
// own. So a Stream that asked ListFiles for a size would hold one slot while
// queueing for a second, and acquireFS deliberately never times out: with
// enough concurrent streams every slot ends up held by a reader whose owner is
// waiting for a slot that can never free, and every filesystem operation in
// Core stops. The public Solder mirror is one of the callers.
//
// The assertion is therefore "exactly one acquisition", expressed as the call
// counts, because the deadlock itself only appears under concurrency and a
// timing test for it would be slow and flaky.
func TestCoreStorageStreamAcquiresOneSlotOnly(t *testing.T) {
	inner := &countingProvider{t: t}
	p := NewCoreStorageProvider(inner)

	rc, size, err := p.Stream(context.Background(), "modpacks/pack.mrpack")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer rc.Close()

	if inner.getFileCalls != 1 {
		t.Errorf("GetFile calls = %d, want 1", inner.getFileCalls)
	}
	if inner.listFilesCalls != 0 {
		t.Fatalf("ListFiles calls = %d, want 0: Stream must not take a second filesystem slot "+
			"while the reader it just returned is still holding one", inner.listFilesCalls)
	}
	if size != SizeUnknown {
		t.Errorf("size = %d, want SizeUnknown: the size cannot be learned without a second acquisition", size)
	}
}

// TestCoreStorageStatStillListsForCallersThatCanAfford it: Stat is not the
// problem and stays as it was. It holds no reader, so its single acquisition
// is safe - only nesting it inside an open stream was not.
func TestCoreStorageStatStillLists(t *testing.T) {
	inner := &countingProvider{t: t}
	p := NewCoreStorageProvider(inner)

	if _, _, err := p.Stat(context.Background(), "modpacks/pack.mrpack"); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if inner.listFilesCalls != 1 {
		t.Fatalf("ListFiles calls = %d, want 1", inner.listFilesCalls)
	}
}
