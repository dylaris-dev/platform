package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// failingReader yields some bytes and then fails, which is what the archive
// pipe does when the walk dies partway or the source disk gives out.
type failingReader struct{ left int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.left <= 0 {
		return 0, errors.New("source went away")
	}
	n := len(p)
	if n > f.left {
		n = f.left
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	f.left -= n
	return n, nil
}

func localStorage(t *testing.T, base string) storageInfo {
	t.Helper()
	cfg, err := json.Marshal(localCfg{BasePath: base})
	if err != nil {
		t.Fatal(err)
	}
	return storageInfo{Provider: "local", Config: cfg}
}

// The fact the cleanup depends on: local, shared and node-local io.Copy STRAIGHT
// into the destination, so an upload that dies partway leaves an archive-shaped
// file behind and returns the error.
//
// It matters because nothing else would ever remove it. Retention prunes
// successful runs only, and reapAbandonedRuns deliberately deletes nothing. On
// node-local the node reports usage by summing every regular file in
// .dylaris-backups/, so the debris counts against the per-server quota and shows
// as used space while appearing in no backup list - and a disk that filled up
// mid-backup leaves the biggest leftover of all, which is then what refuses the
// next run.
func TestAFailedUploadLeavesTheArchiveBehind(t *testing.T) {
	base := t.TempDir()
	key := "backups/server-1/job-2/run.tar.gz"
	info := localStorage(t, base)

	err := uploadBackup(context.Background(), nil, "server-1", info, key, &failingReader{left: 4096}, nil)
	if err == nil {
		t.Fatal("uploadBackup reported success on a reader that failed")
	}

	full := filepath.Join(base, filepath.Clean("/"+key))
	st, statErr := os.Stat(full)
	if statErr != nil {
		t.Fatalf("expected a partial archive at %s: %v", full, statErr)
	}
	if st.Size() == 0 {
		t.Fatal("expected the partial archive to hold the bytes that were copied before the failure")
	}

	// And the cleanup RunBackup now performs on that same key removes it.
	deleteBackup(context.Background(), nil, "server-1", info, key)
	if _, statErr := os.Stat(full); !os.IsNotExist(statErr) {
		t.Fatalf("the partial archive survived the cleanup: %v", statErr)
	}
}

// The cleanup runs on every failure, including one where the upload never got
// as far as creating anything, so it has to be a no-op on a key that is not
// there rather than an error path of its own.
func TestCleaningUpAKeyThatWasNeverWrittenIsHarmless(t *testing.T) {
	base := t.TempDir()
	info := localStorage(t, base)
	deleteBackup(context.Background(), nil, "server-1", info, "backups/never/written.tar.gz")

	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("cleanup created %d entries in the storage root", len(entries))
	}
}

// A successful upload must survive the same key being written and read back -
// the guard against a cleanup that is too eager and removes the archive it just
// stored.
func TestASuccessfulUploadKeepsItsArchive(t *testing.T) {
	base := t.TempDir()
	key := "backups/server-1/job-2/good.tar.gz"
	info := localStorage(t, base)

	if err := uploadBackup(context.Background(), nil, "server-1", info, key, io.LimitReader(&failingReader{left: 1 << 20}, 512), nil); err != nil {
		t.Fatalf("uploadBackup: %v", err)
	}
	if st, err := os.Stat(filepath.Join(base, filepath.Clean("/"+key))); err != nil || st.Size() != 512 {
		t.Fatalf("archive missing or truncated: %v", err)
	}
}
