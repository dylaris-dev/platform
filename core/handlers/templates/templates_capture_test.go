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
	return &models.SubServerInstall{InstallerType: "paper", McVersion: "1.21.4", BuildVersion: "232"}, nil
}
