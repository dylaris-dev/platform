package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"github.com/pkg/sftp"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/metadata"
)

// G31/G34: Beam's save and SFTP's write and truncate opened the target with a
// blocking open, so a named pipe the tenant planted held the handler for good.
// The installer's BungeeCord config edit did the same through O_RDWR + read.
func TestFileWritesRefuseATenantPlantedPipe(t *testing.T) {
	const uuid = "11111111-1111-1111-1111-111111111111"
	refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
	cases := []struct {
		name string
		run  func(bs *beamServer, ctx context.Context, serverDir string) error
	}{
		{"beam save", func(bs *beamServer, ctx context.Context, _ string) error {
			resp, err := bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "survival/config.yml", Content: "x: 1"})
			if err == nil && resp.GetSuccess() {
				return nil
			}
			return errRefusedForTest
		}},
		{"sftp write", func(bs *beamServer, _ context.Context, _ string) error {
			w, err := newVirtualFS(refs, bs.storageMgr, nil, "alice").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/survival/config.yml", Flags: 0x02 | 0x08 | 0x10})
			if err == nil {
				w.(io.Closer).Close()
			}
			return err
		}},
		{"sftp truncate", func(bs *beamServer, _ context.Context, _ string) error {
			r := &sftp.Request{Method: "Setstat", Filepath: "s/survival/config.yml", Flags: 0x01, Attrs: make([]byte, 8)}
			return newVirtualFS(refs, bs.storageMgr, nil, "alice").Filecmd(r)
		}},
		{"installer bungee config", func(_ *beamServer, _ context.Context, serverDir string) error {
			sub, err := os.OpenRoot(filepath.Join(serverDir, "survival"))
			if err != nil {
				return err
			}
			defer sub.Close()
			_, err = addBungeeProxyProtocol(sub)
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bs, _, ctx := newTestBeamServer(t)
			serverDir := bs.storageMgr.GetServerDir(uuid)
			p := filepath.Join(serverDir, "survival", "config.yml")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(p, 0o644); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- c.run(bs, ctx, serverDir) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("wrote into a named pipe as if it were a file")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("blocked on a named pipe")
			}
		})
	}
}

func ownerUID(t *testing.T, p string) uint32 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Uid
}

// G29: a resumed Beam temp and an SFTP open were handed to the container's uid
// by fd, without the hard-link check chownForMCIn makes, so a second name the
// tenant linked to a node file gave the container that file.
func TestUploadsDoNotHandOverAHardLinkedFile(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to see a chown happen")
	}
	t.Setenv("MC_RUN_AS", "1000")
	const uuid = "11111111-1111-1111-1111-111111111111"
	setup := func(t *testing.T, link string) (*beamServer, context.Context, string) {
		bs, _, ctx := newTestBeamServer(t)
		serverDir := bs.storageMgr.GetServerDir(uuid)
		if err := os.MkdirAll(filepath.Join(serverDir, "survival"), 0o755); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(serverDir, ".node_config.json")
		if err := os.WriteFile(victim, []byte(`{"image":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(victim, filepath.Join(serverDir, "survival", link)); err != nil {
			t.Skipf("link: %v", err)
		}
		return bs, ctx, victim
	}

	t.Run("beam resume", func(t *testing.T) {
		bs, ctx, victim := setup(t, ".beam-upload-X")
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-beam-upload-id", "X"))
		stream := &fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "a.jar", int64(len(`{"image":"x"}`)))}
		bs.UploadFile(stream)
		if uid := ownerUID(t, victim); uid != 0 {
			t.Errorf("the linked node file now belongs to uid %d", uid)
		}
	})
	t.Run("sftp write", func(t *testing.T) {
		bs, _, victim := setup(t, "a.yml")
		refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
		w, err := newVirtualFS(refs, bs.storageMgr, nil, "alice").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/survival/a.yml", Flags: 0x02 | 0x08})
		if err == nil {
			w.(io.Closer).Close()
		}
		if uid := ownerUID(t, victim); uid != 0 {
			t.Errorf("the linked node file now belongs to uid %d", uid)
		}
	})
}

// G05: .dylaris.json was written through a plain WriteFile of a fixed temp
// name in the tenant's /data, so a link planted there was followed.
func TestServerMetadataDoesNotFollowAPlantedLink(t *testing.T) {
	base := t.TempDir()
	serverDir := filepath.Join(base, "srv")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, ".node_id")
	if err := os.WriteFile(outside, []byte("node-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(serverDir, ".node_config.json")
	if err := os.WriteFile(inside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"../.node_id", ".node_config.json"} {
		tmp := filepath.Join(serverDir, ".dylaris.json.tmp")
		os.Remove(tmp)
		if err := os.Symlink(target, tmp); err != nil {
			t.Fatal(err)
		}
		writeServerMetadata(serverDir, ServerMetadata{ServerUUID: "u", Name: "n"})
		if b, _ := os.ReadFile(outside); string(b) != "node-1" {
			t.Errorf("link to %s: .node_id now holds %q", target, b)
		}
		if b, _ := os.ReadFile(inside); string(b) != "{}" {
			t.Errorf("link to %s: .node_config.json now holds %q", target, b)
		}
		if m, err := readServerMetadata(serverDir); err != nil || m.Name != "n" {
			t.Errorf("link to %s: metadata not written: %+v %v", target, m, err)
		}
	}
}
