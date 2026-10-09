package main

import (
	"os"
	"path/filepath"
	"testing"

	pb "dylaris-proto/beam"
)

// Beam served the node-local backups to anyone with files.read; the file
// manager and SFTP refuse them without backups.read. A backup is downloaded
// through Core, which checks that permission.
func TestBeamDoesNotServeTheBackupStore(t *testing.T) {
	bs, uuid, ctx := newTestBeamServer(t)
	dir := bs.storageMgr.GetServerDir(uuid)
	for name, body := range map[string]string{
		".dylaris-backups/job-1/a.tar.gz": "ARCHIVE",
		"survival/ok.txt":                 "ok",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const archive = ".dylaris-backups/job-1/a.tar.gz"

	if resp, err := bs.ReadFileContent(ctx, &pb.BeamFileReadReq{Path: archive}); err == nil && resp.Success {
		t.Errorf("read a backup: %q", resp.Content)
	}
	st := &fakeBeamDownloadStream{ctx: ctx}
	if err := bs.DownloadFile(&pb.BeamDownloadReq{Path: archive}, st); err == nil || st.buf.Len() != 0 {
		t.Errorf("downloaded a backup: %v, %d bytes", err, st.buf.Len())
	}
	for _, p := range []string{".dylaris-backups", ".dylaris-backups/job-1"} {
		if resp, _ := bs.ListFiles(ctx, &pb.BeamFileListReq{Path: p}); len(resp.GetFiles()) != 0 {
			t.Errorf("listed %s: %v", p, resp.GetFiles())
		}
	}

	if resp, err := bs.ReadFileContent(ctx, &pb.BeamFileReadReq{Path: "survival/ok.txt"}); err != nil || !resp.Success {
		t.Errorf("an ordinary file is no longer readable: %v %+v", err, resp)
	}
}
