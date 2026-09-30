package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	pb "dylaris-proto/beam"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeBeamDownloadStream is a minimal grpc.ServerStreamingServer for the two
// zip-streaming RPCs. They only call Context and Send, so the embedded nil
// ServerStream is never dereferenced.
type fakeBeamDownloadStream struct {
	grpc.ServerStream
	ctx     context.Context
	chunks  []*pb.BeamChunk
	buf     bytes.Buffer
	sendErr error
}

func (f *fakeBeamDownloadStream) Context() context.Context { return f.ctx }

func (f *fakeBeamDownloadStream) Send(c *pb.BeamChunk) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	// Copy: streamZip reuses one buffer across sends, so retaining the slice
	// would alias the next chunk and make the assembled archive garbage. A test
	// that shared the buffer would "pass" while hiding a real aliasing bug.
	cp := make([]byte, len(c.Data))
	copy(cp, c.Data)
	f.chunks = append(f.chunks, &pb.BeamChunk{
		Data: cp, Offset: c.Offset, Filename: c.Filename, TotalSize: c.TotalSize,
	})
	f.buf.Write(cp)
	return nil
}

// zipEntries returns the archive's entry names, sorted, plus the content of each
// regular file keyed by name.
func zipEntries(t *testing.T, raw []byte) ([]string, map[string]string) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("archive does not parse as a zip (%d bytes): %v", len(raw), err)
	}
	var names []string
	bodies := map[string]string{}
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s in zip: %v", f.Name, err)
		}
		var sb bytes.Buffer
		if _, err := sb.ReadFrom(rc); err != nil {
			t.Fatalf("read %s in zip: %v", f.Name, err)
		}
		rc.Close()
		bodies[f.Name] = sb.String()
	}
	sort.Strings(names)
	return names, bodies
}

// seedTree lays down a small server directory and returns its root.
func seedTree(t *testing.T, bs *beamServer, serverUUID string) string {
	t.Helper()
	root := bs.storageMgr.GetServerDir(serverUUID)
	mk := func(rel, body string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mk("server.properties", "motd=hi")
	mk("plugins/WorldEdit/config.yml", "we: yes")
	mk("plugins/other.jar", "JAR")
	return root
}

func TestDownloadFile_ZipsDirectory(t *testing.T) {
	bs, serverUUID, ctx := newTestBeamServer(t)
	seedTree(t, bs, serverUUID)
	st := &fakeBeamDownloadStream{ctx: ctx}

	if err := bs.DownloadFile(&pb.BeamDownloadReq{Path: "plugins", ZipIfDir: true}, st); err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}

	names, bodies := zipEntries(t, st.buf.Bytes())
	want := []string{"WorldEdit/", "WorldEdit/config.yml", "other.jar"}
	if len(names) != len(want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("entries = %v, want %v", names, want)
		}
	}
	if bodies["other.jar"] != "JAR" {
		t.Errorf("other.jar = %q, want %q", bodies["other.jar"], "JAR")
	}
	if len(st.chunks) == 0 || st.chunks[0].Filename != "plugins.zip" {
		t.Errorf("first chunk filename = %q, want plugins.zip", st.chunks[0].Filename)
	}
	// A streamed archive has no length up front; 0 is the client's "unknown".
	if st.chunks[0].TotalSize != 0 {
		t.Errorf("TotalSize = %d, want 0 (unknown for a streamed zip)", st.chunks[0].TotalSize)
	}
}

// TestDownloadFile_DirectoryWithoutZipFlag pins that a directory is refused
// rather than silently answered with something the caller did not ask for.
func TestDownloadFile_DirectoryWithoutZipFlag(t *testing.T) {
	bs, serverUUID, ctx := newTestBeamServer(t)
	seedTree(t, bs, serverUUID)
	st := &fakeBeamDownloadStream{ctx: ctx}

	err := bs.DownloadFile(&pb.BeamDownloadReq{Path: "plugins", ZipIfDir: false}, st)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
	if st.buf.Len() != 0 {
		t.Errorf("refused request still streamed %d bytes", st.buf.Len())
	}
}

func TestDownloadSelective(t *testing.T) {
	tests := []struct {
		name      string
		basePath  string
		selected  []string
		selectAll bool
		want      []string
	}{
		{
			name:     "single file",
			basePath: "",
			selected: []string{"server.properties"},
			want:     []string{"server.properties"},
		},
		{
			name:     "folder pulls its whole subtree",
			basePath: "plugins",
			selected: []string{"WorldEdit"},
			want:     []string{"WorldEdit/", "WorldEdit/config.yml"},
		},
		{
			name:     "mixed selection",
			basePath: "plugins",
			selected: []string{"WorldEdit", "other.jar"},
			want:     []string{"WorldEdit/", "WorldEdit/config.yml", "other.jar"},
		},
		{
			name:      "select all ignores the selection list",
			basePath:  "plugins",
			selectAll: true,
			want:      []string{"WorldEdit/", "WorldEdit/config.yml", "other.jar"},
		},
		{
			// Names are relative to base_path, so the archive reproduces what the
			// user selected rather than an absolute tree.
			name:     "names are relative to base_path",
			basePath: "",
			selected: []string{"plugins"},
			want:     []string{"plugins/", "plugins/WorldEdit/", "plugins/WorldEdit/config.yml", "plugins/other.jar"},
		},
		{
			// Traversal entries are skipped, and skipping one must not take the
			// legitimate sibling down with it.
			name:     "traversal entries are dropped, valid ones survive",
			basePath: "plugins",
			selected: []string{"../../../../etc/passwd", "other.jar"},
			want:     []string{"other.jar"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bs, serverUUID, ctx := newTestBeamServer(t)
			seedTree(t, bs, serverUUID)
			st := &fakeBeamDownloadStream{ctx: ctx}

			req := &pb.BeamSelectiveReq{BasePath: tt.basePath, Selected: tt.selected, SelectAll: tt.selectAll}
			if err := bs.DownloadSelective(req, st); err != nil {
				t.Fatalf("DownloadSelective: %v", err)
			}
			names, _ := zipEntries(t, st.buf.Bytes())
			if len(names) != len(tt.want) {
				t.Fatalf("entries = %v, want %v", names, tt.want)
			}
			for i := range tt.want {
				if names[i] != tt.want[i] {
					t.Fatalf("entries = %v, want %v", names, tt.want)
				}
			}
		})
	}
}

func TestDownloadSelective_EmptySelectionRejected(t *testing.T) {
	bs, serverUUID, ctx := newTestBeamServer(t)
	seedTree(t, bs, serverUUID)
	st := &fakeBeamDownloadStream{ctx: ctx}

	err := bs.DownloadSelective(&pb.BeamSelectiveReq{BasePath: "", SelectAll: false}, st)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

// TestZipDownload_SymlinkEscapingRootIsOmitted is the security case.
//
// filepath.Walk reports links via Lstat, but os.Open FOLLOWS them, so a link
// planted inside the server directory would otherwise have its target read and
// written into the archive - an arbitrary-file-read out of the tenant's
// directory, dressed up as a folder download. The contained link in the same
// tree must still be archived, otherwise the guard is just "drop all symlinks"
// and would break legitimate layouts.
func TestZipDownload_SymlinkEscapingRootIsOmitted(t *testing.T) {
	bs, serverUUID, ctx := newTestBeamServer(t)
	root := seedTree(t, bs, serverUUID)

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOP SECRET"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "plugins", "escape.txt")); err != nil {
		// Windows needs developer mode or elevation for symlinks; the guard is
		// Linux-relevant (that is where nodes run) and CI covers it there.
		if runtime.GOOS == "windows" || errors.Is(err, os.ErrPermission) {
			t.Skipf("cannot create symlinks here: %v", err)
		}
		t.Fatal(err)
	}
	// A link that stays inside the tenant's own directory is legitimate.
	if err := os.Symlink(filepath.Join("..", "server.properties"), filepath.Join(root, "plugins", "inside.txt")); err != nil {
		t.Fatal(err)
	}

	st := &fakeBeamDownloadStream{ctx: ctx}
	if err := bs.DownloadFile(&pb.BeamDownloadReq{Path: "plugins", ZipIfDir: true}, st); err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}

	names, bodies := zipEntries(t, st.buf.Bytes())
	for _, n := range names {
		if n == "escape.txt" {
			t.Fatalf("symlink to %s was archived; entries=%v", outside, names)
		}
	}
	for _, body := range bodies {
		if body == "TOP SECRET" {
			t.Fatalf("content from outside the server directory leaked into the archive")
		}
	}
	if bodies["inside.txt"] != "motd=hi" {
		t.Errorf("contained symlink was dropped or wrong: inside.txt = %q, want %q", bodies["inside.txt"], "motd=hi")
	}
}

// TestZipDownload_SendErrorStopsTheWalk pins that a client disconnecting
// mid-transfer surfaces as an error instead of the producing goroutine blocking
// forever on a pipe nobody reads.
func TestZipDownload_SendErrorStopsTheWalk(t *testing.T) {
	bs, serverUUID, ctx := newTestBeamServer(t)
	seedTree(t, bs, serverUUID)

	sentinel := errors.New("client went away")
	st := &fakeBeamDownloadStream{ctx: ctx, sendErr: sentinel}

	err := bs.DownloadFile(&pb.BeamDownloadReq{Path: "plugins", ZipIfDir: true}, st)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the send error to propagate", err)
	}
}
