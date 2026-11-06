package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"github.com/pkg/sftp"
)

// L25: the guards read a backslash as part of a name on Linux, the write scope
// rewrote it to a separator, so a name every guard passed was opened as the
// node's own file in the server root.
func TestABackslashNameWritesNoReservedFile(t *testing.T) {
	const cfg = `{"image":"node"}`
	const name = `x\..\.node_config.json`
	for _, tc := range []struct {
		label string
		write func(t *testing.T) string // returns the server dir
	}{
		{"beam save", func(t *testing.T) string {
			bs, uuid, ctx := newTestBeamServer(t)
			dir := bs.storageMgr.GetServerDir(uuid)
			seedConfig(t, dir, cfg)
			bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: name, Content: "PWNED"})
			return dir
		}},
		{"beam upload", func(t *testing.T) string {
			bs, uuid, ctx := newTestBeamServer(t)
			dir := bs.storageMgr.GetServerDir(uuid)
			seedConfig(t, dir, cfg)
			bs.UploadFile(&fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("", name, 5, []byte("PWNED"))})
			return dir
		}},
		{"sftp write", func(t *testing.T) string {
			sm := NewStorageManager(t.TempDir(), nil)
			const uuid = "25252525-2525-2525-2525-252525252525"
			dir := sm.GetServerDir(uuid)
			seedConfig(t, dir, cfg)
			refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
			w, err := newVirtualFS(refs, sm, nil, "alice").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/" + name, Flags: 0x02 | 0x08 | 0x10})
			if err == nil {
				w.WriteAt([]byte("PWNED"), 0)
				w.(io.Closer).Close()
			}
			return dir
		}},
		{"panel write scope", func(t *testing.T) string {
			dir := t.TempDir()
			seedConfig(t, dir, cfg)
			root, leaf, err := openWriteScope(dir, name, false)
			if err == nil {
				if f, err := root.OpenFile(leaf, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o644); err == nil {
					f.WriteString("PWNED")
					f.Close()
				}
				root.Close()
			}
			return dir
		}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			dir := tc.write(t)
			if got, _ := os.ReadFile(filepath.Join(dir, ".node_config.json")); string(got) != cfg {
				t.Fatalf(".node_config.json = %q, written through %q", got, name)
			}
		})
	}
}

func seedConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".node_config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
