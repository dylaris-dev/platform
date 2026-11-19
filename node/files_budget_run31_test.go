package main

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"dylaris-pkg/fileperms"

	"github.com/pkg/sftp"
)

// R31-07: an open SFTP handle's written bytes sat only on
// uploadServerInflight, never in the server's streamed count, so a
// measurement taken mid-upload held them as well and every check adding the
// in-flight count counted them twice; the close then noted the whole file on
// top of a gauge that already held part of it.
func TestSFTPWrittenBytesCountOnceAgainstTheServerLimit(t *testing.T) {
	const uuid = "31073107-3107-3107-3107-310731073107"
	for _, tc := range []struct {
		name  string
		flags uint32
		// seed is what the file holds before the open; more is written after
		// the mid-upload measurement, before the close.
		seed, more int
	}{
		{"new file", 0x02 | 0x08 | 0x10, 0, 0},
		{"truncating overwrite", 0x02 | 0x08 | 0x10, 50, 0},
		{"append past the old end", 0x02 | 0x08, 50, 0},
		// R32-06: measured while the new file is still shorter than the old.
		{"truncating overwrite measured below the old size", 0x02 | 0x08 | 0x10, 500, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDiskBudget(t, 1<<20, 1000)
			_, rdb := gaugeRedis(t)
			sm, _ := newPlacementManager(t, 1)
			sm.rdb = rdb
			prevMgr := globalStorageMgr
			globalStorageMgr = sm
			t.Cleanup(func() { globalStorageMgr = prevMgr })
			dir := sm.GetServerDir(uuid)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.seed > 0 {
				if err := os.WriteFile(dir+"/w.bin", make([]byte, tc.seed), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			recordDiskLimit(ctx, rdb, uuid, 1)
			rdb.Set(ctx, diskGaugeKey(uuid), `{"total":0,"limit":1048576}`, 10*time.Minute)
			t.Cleanup(func() {
				unmeasured.Lock()
				delete(unmeasured.m, uuid)
				delete(unmeasured.last, uuid)
				unmeasured.Unlock()
			})
			remeasureServer(uuid)
			base := gaugeTotal(t, rdb, uuid)

			refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
			w, err := newVirtualFS(refs, sm, rdb, "alice").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/w.bin", Flags: tc.flags})
			if err != nil {
				t.Fatal(err)
			}
			off := int64(0)
			if tc.flags&0x10 == 0 {
				off = int64(tc.seed)
			}
			if _, err := w.WriteAt(make([]byte, 200), off); err != nil {
				t.Fatal(err)
			}
			// What the measurement sees on disk minus what is reserved, plus
			// what a truncating open freed: the close notes only the growth
			// past it, so the gauge keeps it until then, as without a
			// measurement.
			want := dirSize(dir) - 200
			if tc.flags&0x10 != 0 {
				want += int64(tc.seed)
			}
			if got := uploadServerInflight(uuid).Load(); got != 200 {
				t.Fatalf("%d held by the open handle, want 200", got)
			}
			remeasureServer(uuid)
			if got := gaugeTotal(t, rdb, uuid); got != want {
				t.Errorf("gauge %d mid-upload, want %d: the 200 written bytes are also held by the handle", got, want)
			}
			if _, left, ok := serverDiskHeadroom(dir); !ok || left != 1<<20-want {
				t.Errorf("headroom %d (ok %v) mid-upload, want %d", left, ok, 1<<20-want)
			}
			if tc.more > 0 {
				if _, err := w.WriteAt(make([]byte, tc.more), off+200); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.(io.Closer).Close(); err != nil {
				t.Fatal(err)
			}
			if got, onDisk := gaugeTotal(t, rdb, uuid), dirSize(dir); got != onDisk {
				t.Errorf("gauge %d after the close, want what is on disk, %d (base %d)", got, onDisk, base)
			}
			if got := uploadServerInflight(uuid).Load(); got != 0 {
				t.Errorf("%d still held after the close", got)
			}
			if got := serverStreamedOf(uuid).live.Load(); got != 0 {
				t.Errorf("%d bytes still counted as streamed after the close", got)
			}
		})
	}
}
