package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// runtimeFakeStore embeds store.Store (nil) so it satisfies the full interface;
// anything the handler is not supposed to reach panics rather than passing
// quietly. The point of this endpoint is what it does NOT touch.
type runtimeFakeStore struct {
	store.Store
	srv *models.Server

	gotImage string
	gotFlags string
	written  bool
	// setupWritten records a call to the wide setup writer. It must stay false:
	// that one rewrites the installer triple alongside the runtime, and the
	// Content tab reads the triple to decide mods versus plugins.
	setupWritten bool
}

func (f *runtimeFakeStore) GetServerByID(int) (*models.Server, error) { return f.srv, nil }

func (f *runtimeFakeStore) UpdateServerRuntime(_ int, javaImage, extraJvmFlags string) error {
	f.written = true
	f.gotImage, f.gotFlags = javaImage, extraJvmFlags
	return nil
}

func (f *runtimeFakeStore) UpdateServerSetup(int, string, string, string, string, string, string, string) error {
	f.setupWritten = true
	return nil
}

func (f *runtimeFakeStore) GetServerAuditState(int) (bool, bool, int, error) {
	return false, false, 0, nil
}

func runtimeRequest(t *testing.T, fs *runtimeFakeStore, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := &ServerHandler{state: &AppState{Store: fs, Events: services.NewSystemEventsPublisher(nil)}}
	req := httptest.NewRequest(http.MethodPatch, "/api/servers/7/runtime", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), "userID", "alice")
	ctx = context.WithValue(ctx, "isAdmin", true)
	req = mux.SetURLVars(req.WithContext(ctx), map[string]string{"id": "7"})
	rw := httptest.NewRecorder()
	h.UpdateServerRuntime(rw, req)
	return rw
}

func onlineServer() *models.Server {
	return &models.Server{
		ID: 7, UUID: "srv-uuid", OwnerID: "alice", NodeID: 3,
		Status: "stopped", ActiveSubServer: "survival",
		GameImage: "eclipse-temurin:21-jre", InstallerType: "modpack", MinecraftVersion: "1.20.1",
	}
}

// The whole reason this endpoint exists: a settings change must not re-run the
// installer, and must not rewrite the loader metadata on its way past.
func TestUpdateServerRuntimeWritesOnlyTheRuntime(t *testing.T) {
	fs := &runtimeFakeStore{srv: onlineServer()}
	rw := runtimeRequest(t, fs, `{"javaImage":"ghcr.io/dylaris-dev/platform-mc-java25:latest","extraJvmFlags":"  -Xmx1G  "}`)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rw.Code, rw.Body.String())
	}
	if !fs.written {
		t.Fatal("the runtime was not persisted")
	}
	if fs.setupWritten {
		t.Error("the wide setup writer was called; it would rewrite the installer metadata too")
	}
	if fs.gotImage != "ghcr.io/dylaris-dev/platform-mc-java25:latest" {
		t.Errorf("image = %q, want the one that was sent", fs.gotImage)
	}
	if fs.gotFlags != "-Xmx1G" {
		t.Errorf("flags = %q, want them trimmed", fs.gotFlags)
	}
}

// An empty image falls back to what the server already runs. Docker accepts an
// empty image and builds a container with nothing in it, so writing one through
// would produce a server that starts and cannot run java.
func TestUpdateServerRuntimeKeepsTheCurrentImageWhenNoneIsSent(t *testing.T) {
	fs := &runtimeFakeStore{srv: onlineServer()}
	rw := runtimeRequest(t, fs, `{"extraJvmFlags":"-Xmx2G"}`)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rw.Code, rw.Body.String())
	}
	if fs.gotImage != "eclipse-temurin:21-jre" {
		t.Errorf("image = %q, want the server's current one", fs.gotImage)
	}
}

func TestUpdateServerRuntimeRefusesWhatItCannotApply(t *testing.T) {
	tests := []struct {
		name string
		srv  *models.Server
		body string
		want int
	}{
		{
			// There is no start command to rebuild and no container to recreate.
			name: "a server that was never set up",
			srv:  &models.Server{ID: 7, Status: "pending_setup", ActiveSubServer: ""},
			body: `{"javaImage":"x"}`,
			want: http.StatusBadRequest,
		},
		{
			name: "no active sub-server",
			srv:  &models.Server{ID: 7, Status: "stopped", ActiveSubServer: "", GameImage: "img"},
			body: `{"javaImage":"x"}`,
			want: http.StatusBadRequest,
		},
		{
			name: "malformed body",
			srv:  onlineServer(),
			body: `{`,
			want: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &runtimeFakeStore{srv: tt.srv}
			rw := runtimeRequest(t, fs, tt.body)
			if rw.Code != tt.want {
				t.Fatalf("status = %d, want %d (body: %s)", rw.Code, tt.want, rw.Body.String())
			}
			if fs.written {
				t.Error("a refused request still persisted the runtime")
			}
		})
	}
}

// A queue that is not configured must not make the save look like a failure:
// the row is the reconciler's source of truth, so the settings still apply.
func TestUpdateServerRuntimeSucceedsWithoutAQueue(t *testing.T) {
	fs := &runtimeFakeStore{srv: onlineServer()}
	rw := runtimeRequest(t, fs, `{"javaImage":"ghcr.io/dylaris-dev/platform-mc-java25:latest"}`)

	var body map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["success"] != true {
		t.Fatalf("body = %v, want success", body)
	}
}
