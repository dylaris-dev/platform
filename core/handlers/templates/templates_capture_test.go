package templates

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	pb "dylaris-proto/node"
)

func (f *fakeStore) GetServerByID(id int) (*models.Server, error) {
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return nil, sql.ErrNoRows
}
func (f *fakeStore) GetServerGrant(int, string) (*store.ServerGrant, error)     { return nil, nil }
func (f *fakeStore) GetAccountGrant(string, string) (*store.ServerGrant, error) { return nil, nil }

// The zip a node sends for a capture goes through the same extractor as an upload.
func TestExtractCaptureZip(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	z := makeZip(t, []zipEntry{
		{name: "plugins/a.jar", body: []byte("jar")},
		{name: "plugins/a/config.yml", body: []byte("k: v")},
		{name: "world/level.dat", body: []byte("nbt")},
		{name: "plugins/link", body: []byte("/etc/passwd"), symlink: true},
	})
	p := t.TempDir() + "/capture.zip"
	if err := os.WriteFile(p, z, 0o600); err != nil {
		t.Fatal(err)
	}
	skipped, err := extractTemplateZip(context.Background(), h.state, tpl, p, services.TemplateSettings{ContentPolicy: services.TemplatePolicyNoJars})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(storedFiles(t, h, tpl), ","); got != "plugins/a/config.yml,world/level.dat" {
		t.Errorf("stored %q", got)
	}
	got := map[string]bool{}
	for _, s := range skipped {
		got[s.Path] = true
	}
	if len(skipped) != 2 || !got["plugins/a.jar"] || !got["plugins/link"] {
		t.Errorf("skipped %v, want plugins/a.jar and plugins/link", skipped)
	}
}

func TestCaptureNeedsFilesRead(t *testing.T) {
	h, st := newTestHandler(t, nil)
	st.servers = map[int]*models.Server{7: {ID: 7, OwnerID: ownerID, UUID: "u-7"}}
	body := `{"serverId":7,"subServer":"main","selected":["plugins"],"name":"cap","kind":"server"}`
	rec := httptest.NewRecorder()
	h.Capture(rec, asUser(httptest.NewRequest("POST", "/api/templates/capture", strings.NewReader(body)), strangerID, false))
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d (%s), want 403", rec.Code, rec.Body)
	}
	if len(st.templates) != 0 {
		t.Errorf("a denied capture left %d template rows", len(st.templates))
	}
}

func nodeChunks(parts ...[]byte) chan *pb.NodeMessage {
	ch := make(chan *pb.NodeMessage, len(parts)+2)
	ch <- &pb.NodeMessage{Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{Filename: "x.zip"}}}
	for _, p := range parts {
		ch <- &pb.NodeMessage{Payload: &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: p}}}
	}
	return ch
}

func TestWriteNodeStream(t *testing.T) {
	final := &pb.NodeMessage{Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{TotalBytes: 6}}}
	limit := func(n int64) *int64 { return &n }
	tests := []struct {
		name  string
		build func() <-chan *pb.NodeMessage
		max   *int64
		want  string
		err   error
	}{
		{"complete", func() <-chan *pb.NodeMessage {
			ch := nodeChunks([]byte("abc"), []byte("def"))
			ch <- final
			close(ch)
			return ch
		}, limit(10), "abcdef", nil},
		{"no limit", func() <-chan *pb.NodeMessage {
			ch := nodeChunks([]byte("abc"), []byte("def"))
			ch <- final
			close(ch)
			return ch
		}, nil, "abcdef", nil},
		{"over the bound", func() <-chan *pb.NodeMessage {
			ch := nodeChunks([]byte("abc"), []byte("def"))
			ch <- final
			close(ch)
			return ch
		}, limit(5), "", errTooLarge},
		{"closed early", func() <-chan *pb.NodeMessage {
			ch := nodeChunks([]byte("abc"))
			close(ch)
			return ch
		}, limit(10), "", errNodeIncomplete},
		{"node error", func() <-chan *pb.NodeMessage {
			ch := nodeChunks([]byte("abc"))
			ch <- &pb.NodeMessage{Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 404, Message: "gone"}}}
			close(ch)
			return ch
		}, limit(10), "", errNode},
	}
	for _, tc := range tests {
		var buf bytes.Buffer
		err := writeNodeStream(context.Background(), tc.build(), &buf, tc.max)
		if tc.err == nil {
			if err != nil || buf.String() != tc.want {
				t.Errorf("%s: got %q, %v; want %q", tc.name, buf.String(), err, tc.want)
			}
			continue
		}
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.err)
		}
	}
}

func TestWriteNodeStreamStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := writeNodeStream(ctx, make(chan *pb.NodeMessage), &bytes.Buffer{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}

// The owner passes the files.read check; with no node reachable the capture
// fails and must leave no template row behind.
func TestCaptureFailureLeavesNoRow(t *testing.T) {
	h, st := newTestHandler(t, nil)
	st.servers = map[int]*models.Server{7: {ID: 7, OwnerID: ownerID, UUID: "u-7"}}
	body := `{"serverId":7,"subServer":"main","selected":["plugins"],"name":"cap","kind":"server"}`
	rec := httptest.NewRecorder()
	h.Capture(rec, asUser(httptest.NewRequest("POST", "/api/templates/capture", strings.NewReader(body)), ownerID, false))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("got %d (%s), want 500", rec.Code, rec.Body)
	}
	if len(st.templates) != 0 {
		t.Errorf("a failed capture left %d template rows", len(st.templates))
	}
}

func (f *fakeStore) GetSubServerInstall(int, string) (*models.SubServerInstall, error) {
	if f.install != nil {
		return f.install, nil
	}
	return &models.SubServerInstall{InstallerType: "paper", McVersion: "1.21.4", BuildVersion: "232"}, nil
}

// A capture pins the loader the sub-server actually runs, not the Minecraft
// version an online install recorded as its build: otherwise every later
// install takes whatever loader is newest that day.
func TestCaptureFillPinsTheInstalledLoader(t *testing.T) {
	listed := map[string][]*pb.FileInfo{
		"main/libraries/net/minecraftforge/forge":   {{Name: "1.20.1-47.4.28", IsDir: true}},
		"main/libraries/net/fabricmc/fabric-loader": {{Name: "0.20.1", IsDir: true}},
	}
	prev := listServerDir
	listServerDir = func(_ *nodegrpc.Registry, _ int, _ string, dir string) ([]*pb.FileInfo, error) {
		return listed[dir], nil
	}
	t.Cleanup(func() { listServerDir = prev })

	cases := []struct {
		name         string
		rec          models.SubServerInstall
		req          templateMeta
		sw, mc, want string
	}{
		{name: "forge from its libraries", rec: models.SubServerInstall{InstallerType: "forge", McVersion: "1.20.1", BuildVersion: "1.20.1"}, sw: "forge", mc: "1.20.1", want: "47.4.28"},
		{name: "fabric from its libraries", rec: models.SubServerInstall{InstallerType: "fabric", McVersion: "1.21.4", BuildVersion: "1.21.4"}, sw: "fabric", mc: "1.21.4", want: "0.20.1"},
		{name: "recorded loader wins", rec: models.SubServerInstall{InstallerType: "fabric", McVersion: "1.21.4", BuildVersion: "1.21.4", Loader: "0.16.10"}, sw: "fabric", mc: "1.21.4", want: "0.16.10"},
		{name: "nothing found keeps the build", rec: models.SubServerInstall{InstallerType: "neoforge", McVersion: "1.21.1", BuildVersion: "21.1.265"}, sw: "neoforge", mc: "1.21.1", want: "21.1.265"},
		{name: "an upload record names no software", rec: models.SubServerInstall{InstallerType: "upload", BuildVersion: "x"}},
		{name: "a chosen other software takes nothing from the record", rec: models.SubServerInstall{InstallerType: "fabric", McVersion: "1.21.4", BuildVersion: "1.21.4"}, req: templateMeta{Software: "paper"}, sw: "paper"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, st := newTestHandler(t, nil)
			rec := c.rec
			st.install = &rec
			req := captureRequest{templateMeta: c.req, ServerID: 7, SubServer: "main"}
			h.fillFromInstall(&req, &models.Server{ID: 7, UUID: "u-7"})
			if req.Software != c.sw || req.McVersion != c.mc || req.SoftwareVersion != c.want {
				t.Fatalf("got %q %q %q, want %q %q %q", req.Software, req.McVersion, req.SoftwareVersion, c.sw, c.mc, c.want)
			}
		})
	}
}

// Software a template cannot install is refused when the template is saved,
// with the reason, and leaves no row.
func TestCreateRefusesSoftwareTheInstallWould(t *testing.T) {
	for body, want := range map[string]string{
		`{"name":"a","kind":"server","software":"purpur","mcVersion":"1.21.4"}`:         "Purpur is not supported; use paper, which runs the same plugins.",
		`{"name":"a","kind":"server","software":"neoforge","mcVersion":"1.21.1"}`:       "A neoforge template needs the NeoForge version, such as 21.1.265 for Minecraft 1.21.1.",
		`{"name":"a","kind":"server","software":"neoforge","softwareVersion":"1.21.1"}`: `"1.21.1" is a Minecraft version, not a NeoForge version; NeoForge versions look like 21.1.265 (for Minecraft 1.21.1).`,
		`{"name":"a","kind":"server","software":"Quilt"}`:                               `Software "quilt" is not supported; use one of paper, vanilla, fabric, forge, neoforge, velocity, waterfall, bungeecord.`,
	} {
		h, st := newTestHandler(t, nil)
		rec := httptest.NewRecorder()
		h.Create(rec, asUser(httptest.NewRequest("POST", "/api/templates", strings.NewReader(body)), ownerID, false))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), strings.ReplaceAll(want, `"`, `\"`)) {
			t.Errorf("%s: got %d %s, want 400 %q", body, rec.Code, rec.Body, want)
		}
		if len(st.templates) != 0 {
			t.Errorf("%s: a refused create left a row", body)
		}
	}

	h, st := newTestHandler(t, nil)
	rec := httptest.NewRecorder()
	h.Create(rec, asUser(httptest.NewRequest("POST", "/api/templates", strings.NewReader(`{"name":"a","kind":"server","software":"Forge","softwareVersion":"1.20.1-47.3.0"}`)), ownerID, false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	for _, tpl := range st.templates {
		if tpl.Software != "forge" || tpl.McVersion != "1.20.1" || tpl.SoftwareVersion != "47.3.0" {
			t.Errorf("stored %q %q %q, want forge 1.20.1 47.3.0", tpl.Software, tpl.McVersion, tpl.SoftwareVersion)
		}
	}
}

// An edit of the software is checked too; a rename of a template an older
// release let through is not.
func TestUpdateChecksSoftwareOnlyWhenItChanges(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	st.templates[tpl.ID].Software = "purpur"
	patch := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.Update(rec, withID(asUser(httptest.NewRequest("PATCH", "/api/templates/x", strings.NewReader(body)), ownerID, false), tpl.ID))
		return rec
	}
	if rec := patch(`{"name":"renamed"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename: got %d %s", rec.Code, rec.Body)
	}
	if rec := patch(`{"software":"neoforge","softwareVersion":"1.21.1"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("neoforge with a Minecraft version: got %d %s, want 400", rec.Code, rec.Body)
	}
	if rec := patch(`{"software":"neoforge","softwareVersion":"21.1.265"}`); rec.Code != http.StatusOK {
		t.Fatalf("valid neoforge: got %d %s", rec.Code, rec.Body)
	}
	if got := st.templates[tpl.ID]; got.Software != "neoforge" || got.McVersion != "1.21.1" {
		t.Errorf("stored %q %q, want neoforge 1.21.1", got.Software, got.McVersion)
	}
}
