package services

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	backupstorage "dylaris-core/storage/backup"
)

// A presigned part URL does not bound what is sent through it, and a node's
// report of what it sent is a claim. These pin what Core measures itself before
// it signs more parts and before it completes an upload.

// H1: the size the run is given is the one Core measured on completion.
func TestBackupTransfer_CompleteRecordsCoresSizeOnTheRun(t *testing.T) {
	st := newTransferFakeStore()
	st.runs[1].UploadID, st.runs[1].PartSize = "upload-1", backupPartSize
	resp := newTestTransfer(st, &fakeMultipartStorage{}).HandleCompleteUpload(context.Background(), hostingNode, completeMsg("1")).GetCompleteUploadResponse()
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
	if got := st.runs[1].UploadedBytes; got == nil || *got != 3*backupPartSize+17 {
		t.Fatalf("run's uploaded size = %v, want %d recorded by Core", got, 3*backupPartSize+17)
	}
}

// H1: an upload past the owner's allowance is not completed. Checked on the
// listed parts before completing, which aborts, and on the completed object,
// which deletes it, because parts can arrive between the listing and the
// completion.
func TestBackupTransfer_CompleteRefusedOverTheAllowance(t *testing.T) {
	tests := []struct {
		name         string
		listed       int64 // what the parts add up to when listed
		completed    int64 // what the completed object holds
		wantRefused  bool
		wantAborted  bool
		wantDeleted  bool
		wantComplete int
	}{
		{name: "within the allowance", listed: gib / 4, completed: gib / 4, wantComplete: 1},
		{name: "the parts already exceed it", listed: gib, completed: gib, wantRefused: true, wantAborted: true},
		{name: "parts sent after the listing exceed it", listed: gib / 4, completed: gib, wantRefused: true, wantDeleted: true, wantComplete: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newTransferFakeStore()
			st.quotaGB, st.usedBytes = "1", gib/2
			st.runs[1].UploadID, st.runs[1].PartSize = "upload-1", backupPartSize
			prov := &fakeMultipartStorage{
				usage:        backupstorage.MultipartUsage{Parts: 1, Bytes: tt.listed, Largest: tt.listed},
				completeSize: tt.completed,
			}

			resp := newTestTransfer(st, prov).HandleCompleteUpload(context.Background(), hostingNode, completeMsg("1")).GetCompleteUploadResponse()

			if refused := resp.Error == "backup exceeds the backup storage allowance"; refused != tt.wantRefused {
				t.Fatalf("error = %q, want refused=%v", resp.Error, tt.wantRefused)
			}
			if prov.completes != tt.wantComplete {
				t.Errorf("completes = %d, want %d", prov.completes, tt.wantComplete)
			}
			if aborted := len(st.aborts(prov)) > 0; aborted != tt.wantAborted {
				t.Errorf("aborted = %v, want %v", st.aborts(prov), tt.wantAborted)
			}
			if deleted := len(prov.deletes()) > 0; deleted != tt.wantDeleted {
				t.Errorf("deleted = %v, want %v", prov.deletes(), tt.wantDeleted)
			}
			if tt.wantRefused && st.runs[1].UploadedBytes != nil {
				t.Errorf("a refused completion recorded a size")
			}
		})
	}
}

// H1: a run closed while its upload was completing, its upload already
// discarded by whoever closed it, does not keep the archive the completion
// produced.
func TestBackupTransfer_CompleteForARunClosedMeanwhileDeletesTheArchive(t *testing.T) {
	st := newTransferFakeStore()
	st.runs[1].UploadID, st.runs[1].PartSize = "upload-1", backupPartSize
	prov := &fakeMultipartStorage{}
	prov.completeHook = func() {
		st.mu.Lock()
		st.runs[1].Status = "failed"
		st.mu.Unlock()
	}
	resp := newTestTransfer(st, prov).HandleCompleteUpload(context.Background(), hostingNode, completeMsg("1")).GetCompleteUploadResponse()
	if !strings.Contains(resp.Error, "no longer running") {
		t.Fatalf("error = %q, want a no-longer-running refusal", resp.Error)
	}
	if got := prov.deletes(); len(got) != 1 || got[0] != st.runs[1].StorageKey {
		t.Fatalf("deleted = %v, want the run's key", got)
	}
}

// M1: more part URLs are signed only for an upload that is what Core asked for,
// and M2: a request that is served counts as upload activity.
func TestBackupTransfer_PartURLsRefusedWhenTheUploadIsNotAsTold(t *testing.T) {
	tests := []struct {
		name        string
		usage       backupstorage.MultipartUsage
		parts       []int32
		wantError   string
		wantAborted bool
	}{
		{name: "as told", usage: backupstorage.MultipartUsage{Parts: 4, Bytes: 4 * backupPartSize, Largest: backupPartSize}, parts: []int32{5, 6, 7, 8}},
		{name: "a part larger than the part size", usage: backupstorage.MultipartUsage{Parts: 1, Bytes: 5 * gib, Largest: 5 * gib}, parts: []int32{2},
			wantError: "larger than the part size", wantAborted: true},
		{name: "the parts past the allowance", usage: backupstorage.MultipartUsage{Parts: 10, Bytes: 10 * backupPartSize, Largest: backupPartSize}, parts: []int32{11},
			wantError: "backup exceeds the backup storage allowance", wantAborted: true},
		{name: "a part far ahead of the uploaded ones", usage: backupstorage.MultipartUsage{Parts: 2, Bytes: 2 * backupPartSize, Largest: backupPartSize}, parts: []int32{11},
			wantError: "ahead of the 2 uploaded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newTransferFakeStore()
			st.quotaGB, st.usedBytes = "1", gib/2 // headroom 512 MiB: eight parts
			st.runs[1].UploadID, st.runs[1].PartSize = "upload-1", backupPartSize
			prov := &fakeMultipartStorage{usage: tt.usage}

			resp := newTestTransfer(st, prov).HandleUploadPartURLs(context.Background(), hostingNode, partURLsMsg("1", tt.parts...)).GetUploadPartUrlsResponse()

			if tt.wantError == "" {
				if resp.Error != "" || len(resp.Urls) != len(tt.parts) {
					t.Fatalf("error %q urls %d, want %d URLs", resp.Error, len(resp.Urls), len(tt.parts))
				}
				if st.touches != 1 {
					t.Errorf("touches = %d, want the request recorded as upload activity", st.touches)
				}
			} else {
				if !strings.Contains(resp.Error, tt.wantError) || len(resp.Urls) != 0 {
					t.Fatalf("error %q urls %d, want a refusal containing %q", resp.Error, len(resp.Urls), tt.wantError)
				}
				if st.touches != 0 {
					t.Errorf("a refused request was recorded as upload activity")
				}
			}
			if aborted := len(st.aborts(prov)) > 0; aborted != tt.wantAborted {
				t.Errorf("aborted = %v, want %v", st.aborts(prov), tt.wantAborted)
			}
		})
	}
}

// M4: a node that asks about another node's runs cannot fill the log.
func TestBackupTransfer_ForeignAsksAreLoggedOncePerMinute(t *testing.T) {
	var (
		mu  sync.Mutex
		buf strings.Builder
	)
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	tr := newTestTransfer(newTransferFakeStore(), &fakeMultipartStorage{})
	for i := 0; i < 50; i++ {
		tr.HandleUploadPartURLs(context.Background(), otherNode, partURLsMsg("1", 1))
		tr.HandleRestoreURL(context.Background(), otherNode, restoreURLMsg("20"))
	}
	mu.Lock()
	lines := strings.Count(buf.String(), "is hosted by node")
	mu.Unlock()
	if lines != 1 {
		t.Fatalf("logged %d lines about foreign runs for 100 requests in a burst, want 1", lines)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// L1: two runs of one job in the same second get different keys, in the layout
// every reader already accepts.
func TestNewBackupStorageKeyIsUniqueWithinASecond(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	a, b := NewBackupStorageKey("srv-uuid", 10, now), NewBackupStorageKey("srv-uuid", 10, now)
	if a == b {
		t.Fatalf("two keys in the same second are both %q", a)
	}
	for _, k := range []string{a, b} {
		if !strings.HasPrefix(k, "backups/srv-uuid/job-10/20260914-120000-") || !strings.HasSuffix(k, ".tar.gz") {
			t.Errorf("key %q, want backups/srv-uuid/job-10/20260914-120000-<suffix>.tar.gz", k)
		}
	}
}
