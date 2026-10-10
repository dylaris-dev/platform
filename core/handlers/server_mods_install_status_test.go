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

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

type installStatusFakeStore struct {
	store.Store
	upserted *models.ServerMod
	// queuedAtUpsert: whether the node's command stream already existed when
	// the row was written.
	mr             *miniredis.Miniredis
	queuedAtUpsert bool
	statusSet      []string
}

func (f *installStatusFakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: userID}, nil
}

func (f *installStatusFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-uuid", OwnerID: "alice", NodeID: 1, InstallerType: "fabric", ActiveSubServer: "survival"}, nil
}

func (f *installStatusFakeStore) GetServerModByProject(serverID int, sub, projectID string) (*models.ServerMod, error) {
	return nil, nil
}

func (f *installStatusFakeStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id, Token: "node-token"}, nil
}

func (f *installStatusFakeStore) UpsertServerMod(m *models.ServerMod) (int, error) {
	f.upserted = m
	if f.mr != nil {
		f.queuedAtUpsert = f.mr.Exists("dylaris:node:node-token:cmds")
	}
	return 1, nil
}

func (f *installStatusFakeStore) SetServerModStatus(serverID int, sub, projectID, installID, status, message string) (bool, error) {
	f.statusSet = append(f.statusSet, installID+"/"+status)
	return true, nil
}

// The panel decides from this reply whether to wait for the node's report or to
// say "sent" and stop: waiting on a node too old to answer is an endless spinner.
func TestInstallReplyCarriesTheRecordedStatus(t *testing.T) {
	tests := []struct {
		name    string
		version string // "" = no heartbeat
		want    string
	}{
		{name: "reporting node", version: modReportingSince, want: models.ServerModInstalling},
		{name: "node too old to report", version: "2026.08.28", want: models.ServerModInstalled},
		{name: "no heartbeat", version: "", want: models.ServerModInstalled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
			if tt.version != "" {
				seedHeartbeat(t, m, "node-token", tt.version)
			}
			fs := &installStatusFakeStore{mr: m}
			h := &ServerModsHandler{state: &AppState{Store: fs, Redis: rdb, Queue: services.NewQueueService(rdb)}}

			body := `{"projectId":"P1","versionId":"V1","fileName":"spark.jar","downloadUrl":"https://cdn.modrinth.com/data/P1/versions/V1/spark.jar"}`
			req := httptest.NewRequest(http.MethodPost, "/api/servers/7/mods", strings.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), "userID", "alice"))
			rw := httptest.NewRecorder()
			h.Install(rw, mux.SetURLVars(req, map[string]string{"id": "7"}))

			if rw.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rw.Code, rw.Body.String())
			}
			var resp struct {
				Success bool   `json:"success"`
				Status  string `json:"status"`
			}
			if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if !resp.Success || resp.Status != tt.want {
				t.Errorf("reply = %+v, want status %q", resp, tt.want)
			}
			if fs.upserted == nil || fs.upserted.Status != resp.Status {
				t.Errorf("reply status %q does not match the stored row %+v", resp.Status, fs.upserted)
			}
			// Row first, node second: a node fast enough to answer before the
			// upsert had its report dropped as naming an unknown attempt.
			if fs.queuedAtUpsert {
				t.Error("the install_mod command was queued before the row holding its install id was written")
			}
			if !m.Exists("dylaris:node:node-token:cmds") {
				t.Error("the install_mod command was never queued")
			}
		})
	}
}

// A dispatch that fails leaves a row the node never heard about; it must read as
// failed, not sit at installing (or installed) forever.
func TestInstallMarksTheRowFailedWhenDispatchFails(t *testing.T) {
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	m.Close()
	fs := &installStatusFakeStore{}
	h := &ServerModsHandler{state: &AppState{Store: fs, Redis: rdb, Queue: services.NewQueueService(rdb)}}

	body := `{"projectId":"P1","versionId":"V1","fileName":"spark.jar","downloadUrl":"https://cdn.modrinth.com/data/P1/versions/V1/spark.jar"}`
	req := httptest.NewRequest(http.MethodPost, "/api/servers/7/mods", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), "userID", "alice"))
	rw := httptest.NewRecorder()
	h.Install(rw, mux.SetURLVars(req, map[string]string{"id": "7"}))

	if rw.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rw.Code, rw.Body.String())
	}
	if fs.upserted == nil {
		t.Fatal("no row was written")
	}
	want := fs.upserted.InstallID + "/" + models.ServerModFailed
	if len(fs.statusSet) != 1 || fs.statusSet[0] != want {
		t.Errorf("status writes = %v, want [%s]", fs.statusSet, want)
	}
}
