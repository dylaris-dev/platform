package servers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// serverMetadataFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods DeclareServerLoaderMetadata
// touches are overridden. Any other call (e.g. an install/reinstall status or
// desired-state write) would panic - none of these tests trigger one, which is
// itself part of what pins the "never reinstalls" invariant.
type serverMetadataFakeStore struct {
	store.Store

	server    *models.Server
	serverErr error

	updateErr error
	updated   []loaderMetadataUpdate

	// Audit observability: GetServerAuditState reports auditEnabled so
	// LogServerAudit reaches InsertServerAudit (recorded here) instead of
	// no-op'ing early, letting tests assert the audit event without wiring a
	// real store.
	auditEnabled bool
	auditEvents  []*models.ServerAuditEvent
}

type loaderMetadataUpdate struct {
	id               int
	installerType    string
	minecraftVersion string
	buildNumber      string
}

func (f *serverMetadataFakeStore) GetServerByID(id int) (*models.Server, error) {
	return f.server, f.serverErr
}

func (f *serverMetadataFakeStore) UpdateServerLoaderMetadata(id int, installerType, minecraftVersion, buildNumber string) error {
	f.updated = append(f.updated, loaderMetadataUpdate{id, installerType, minecraftVersion, buildNumber})
	return f.updateErr
}

func (f *serverMetadataFakeStore) GetServerAuditState(serverID int) (bool, bool, int, error) {
	return f.auditEnabled, false, 0, nil
}

func (f *serverMetadataFakeStore) InsertServerAudit(ev *models.ServerAuditEvent) error {
	f.auditEvents = append(f.auditEvents, ev)
	return nil
}

// Events is left nil - SystemEventsPublisher.Publish is nil-safe on both the
// receiver and its rdb, so handlers can call it unconditionally in tests.
func newServerMetadataHandler(fs *serverMetadataFakeStore) *ServerHandler {
	return &ServerHandler{state: &handlers.AppState{Store: fs}}
}

func declareMetadataReqWithStore(fs *serverMetadataFakeStore, serverID int, body map[string]interface{}) *httptest.ResponseRecorder {
	h := newServerMetadataHandler(fs)
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("PATCH", "/api/servers/"+strconv.Itoa(serverID)+"/loader-metadata", bytes.NewReader(b))
	r = mux.SetURLVars(r, map[string]string{"id": strconv.Itoa(serverID)})
	rec := httptest.NewRecorder()
	h.DeclareServerLoaderMetadata(rec, r)
	return rec
}

func TestDeclareServerLoaderMetadata_ValidPersists(t *testing.T) {
	fs := &serverMetadataFakeStore{
		server:       &models.Server{ID: 1, Status: "online", InstallerType: "upload", MinecraftVersion: ""},
		auditEnabled: true,
	}
	rec := declareMetadataReqWithStore(fs, 1, map[string]interface{}{
		"installerType":    "paper",
		"minecraftVersion": "1.20.4",
	})
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("response = %+v, want success", resp)
	}
	if len(fs.updated) != 1 {
		t.Fatalf("UpdateServerLoaderMetadata calls = %d, want 1", len(fs.updated))
	}
	got := fs.updated[0]
	if got.id != 1 || got.installerType != "paper" || got.minecraftVersion != "1.20.4" {
		t.Fatalf("update = %+v, want {1 paper 1.20.4 }", got)
	}

	if len(fs.auditEvents) != 1 {
		t.Fatalf("audit events = %d, want 1: %+v", len(fs.auditEvents), fs.auditEvents)
	}
	ev := fs.auditEvents[0]
	if ev.ServerID != 1 || ev.EventType != handlers.ServerAuditEventLoaderMetadataDeclared {
		t.Fatalf("audit event = %+v, want serverID=1 eventType=%q", ev, handlers.ServerAuditEventLoaderMetadataDeclared)
	}
	if ev.Metadata["from_installer_type"] != "upload" || ev.Metadata["to_installer_type"] != "paper" {
		t.Fatalf("audit metadata installer_type = %+v", ev.Metadata)
	}
	if ev.Metadata["from_minecraft_version"] != "" || ev.Metadata["to_minecraft_version"] != "1.20.4" {
		t.Fatalf("audit metadata minecraft_version = %+v", ev.Metadata)
	}
}

func TestDeclareServerLoaderMetadata_NormalizesLoaderCase(t *testing.T) {
	fs := &serverMetadataFakeStore{server: &models.Server{ID: 1, Status: "online"}}
	rec := declareMetadataReqWithStore(fs, 1, map[string]interface{}{
		"installerType":    "  Fabric  ",
		"minecraftVersion": " 1.21 ",
	})
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.updated) != 1 || fs.updated[0].installerType != "fabric" || fs.updated[0].minecraftVersion != "1.21" {
		t.Fatalf("update = %+v, want trimmed+lowercased fabric/1.21", fs.updated)
	}
}

func TestDeclareServerLoaderMetadata_PassesThroughOptionalBuildNumber(t *testing.T) {
	fs := &serverMetadataFakeStore{server: &models.Server{ID: 1, Status: "online"}}
	rec := declareMetadataReqWithStore(fs, 1, map[string]interface{}{
		"installerType":    "neoforge",
		"minecraftVersion": "1.21.1",
		"buildNumber":      "47.2.0",
	})
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.updated) != 1 || fs.updated[0].buildNumber != "47.2.0" {
		t.Fatalf("update = %+v, want buildNumber=47.2.0", fs.updated)
	}
}

// A repeat declare that omits buildNumber must not clobber one set by an
// earlier declare/install - the handler substitutes the server's current
// value before calling the store.
func TestDeclareServerLoaderMetadata_PreservesBuildNumberWhenOmitted(t *testing.T) {
	fs := &serverMetadataFakeStore{server: &models.Server{ID: 1, Status: "online", BuildNumber: "47.2.0"}}
	rec := declareMetadataReqWithStore(fs, 1, map[string]interface{}{
		"installerType":    "neoforge",
		"minecraftVersion": "1.21.1",
	})
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.updated) != 1 || fs.updated[0].buildNumber != "47.2.0" {
		t.Fatalf("update = %+v, want buildNumber preserved as 47.2.0", fs.updated)
	}
}

func TestDeclareServerLoaderMetadata_InvalidLoaderRejected(t *testing.T) {
	cases := []string{"", "bogus", "PAPER-not-lowered-and-unknown", "vanilla", "upload"}
	for _, loader := range cases {
		t.Run("loader="+loader, func(t *testing.T) {
			fs := &serverMetadataFakeStore{server: &models.Server{ID: 1, Status: "online"}}
			rec := declareMetadataReqWithStore(fs, 1, map[string]interface{}{
				"installerType":    loader,
				"minecraftVersion": "1.20.4",
			})
			if rec.Code != 400 {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if len(fs.updated) != 0 {
				t.Fatalf("UpdateServerLoaderMetadata must not be called on invalid input, got %+v", fs.updated)
			}
		})
	}
}

func TestDeclareServerLoaderMetadata_InvalidMcVersionRejected(t *testing.T) {
	cases := []string{"", "not-a-version", "1", "latest", "1.20.4.5.6"}
	for _, v := range cases {
		t.Run("mcVersion="+v, func(t *testing.T) {
			fs := &serverMetadataFakeStore{server: &models.Server{ID: 1, Status: "online"}}
			rec := declareMetadataReqWithStore(fs, 1, map[string]interface{}{
				"installerType":    "paper",
				"minecraftVersion": v,
			})
			if rec.Code != 400 {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if len(fs.updated) != 0 {
				t.Fatalf("UpdateServerLoaderMetadata must not be called on invalid input, got %+v", fs.updated)
			}
		})
	}
}

func TestDeclareServerLoaderMetadata_InvalidJSON(t *testing.T) {
	fs := &serverMetadataFakeStore{server: &models.Server{ID: 1, Status: "online"}}
	h := newServerMetadataHandler(fs)
	r := httptest.NewRequest("PATCH", "/api/servers/1/loader-metadata", bytes.NewReader([]byte("not json")))
	r = mux.SetURLVars(r, map[string]string{"id": "1"})
	rec := httptest.NewRecorder()
	h.DeclareServerLoaderMetadata(rec, r)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestDeclareServerLoaderMetadata_ServerNotFound(t *testing.T) {
	fs := &serverMetadataFakeStore{serverErr: errors.New("no rows")}
	rec := declareMetadataReqWithStore(fs, 99, map[string]interface{}{
		"installerType":    "paper",
		"minecraftVersion": "1.20.4",
	})
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestDeclareServerLoaderMetadata_StoreErrorReturns500(t *testing.T) {
	fs := &serverMetadataFakeStore{server: &models.Server{ID: 1, Status: "online"}, updateErr: errors.New("db down")}
	rec := declareMetadataReqWithStore(fs, 1, map[string]interface{}{
		"installerType":    "paper",
		"minecraftVersion": "1.20.4",
	})
	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
}
