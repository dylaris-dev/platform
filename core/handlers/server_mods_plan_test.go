package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type planFakeStore struct {
	store.Store
	srv  models.Server
	mods []models.ServerMod
}

func (f *planFakeStore) GetServerByID(id int) (*models.Server, error) {
	s := f.srv
	s.ID = id
	return &s, nil
}

func (f *planFakeStore) ListServerMods(int, string) ([]models.ServerMod, error) {
	return f.mods, nil
}

func TestPlanEndpoint(t *testing.T) {
	known := models.Server{InstallerType: "neoforge", MinecraftVersion: "1.21.1", ActiveSubServer: "main"}
	mods := []models.ServerMod{
		{ModrinthProjectID: "OK", ModrinthVersionID: "ok1", Status: "installed"},
		{ModrinthProjectID: "BUSY", ModrinthVersionID: "busy1", Status: "installing"},
		{ModrinthProjectID: "BROKEN", Status: "failed"},
		{Status: "installed"},
	}
	tests := []struct {
		name       string
		srv        models.Server
		planErr    error
		wantStatus int
		wantInMsg  string
	}{
		{name: "planned", srv: known, wantStatus: 200},
		{name: "imported server", srv: models.Server{InstallerType: "upload"}, wantStatus: 409, wantInMsg: "Declare the server's loader"},
		{name: "neoforge without MC", srv: models.Server{InstallerType: "neoforge"}, wantStatus: 409, wantInMsg: "Declare"},
		{name: "root does not fit", srv: known, planErr: &services.ModFitError{Reason: "This build is for forge"}, wantStatus: 409, wantInMsg: "for forge"},
		{name: "unknown version", srv: known, planErr: &services.ModrinthHTTPError{Status: 404}, wantStatus: 400, wantInMsg: "Modrinth has no version"},
		{name: "rate limited", srv: known, planErr: &services.ModrinthHTTPError{Status: 429}, wantStatus: 503},
		{name: "Modrinth down", srv: known, planErr: errors.New("timeout"), wantStatus: 502},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotInstalled map[string]string
			called := false
			old := planModInstall
			t.Cleanup(func() { planModInstall = old })
			planModInstall = func(_ context.Context, versionID, loader, mc string, installed map[string]string) (*services.ModInstallPlan, error) {
				called = true
				gotInstalled = installed
				if versionID != "v1" || loader != "neoforge" || mc != "1.21.1" {
					t.Errorf("plan(%q, %q, %q)", versionID, loader, mc)
				}
				if tt.planErr != nil {
					return nil, tt.planErr
				}
				return &services.ModInstallPlan{
					Install:      []services.PlanEntry{{ProjectID: "A", VersionID: "v1"}},
					Unresolved:   []services.DepProblem{},
					Incompatible: []services.DepProblem{},
				}, nil
			}
			h := NewServerModsHandler(&AppState{Store: &planFakeStore{srv: tt.srv, mods: mods}})
			r := httptest.NewRequest("POST", "/api/servers/7/mods/plan", strings.NewReader(`{"versionId":" v1 "}`))
			r = mux.SetURLVars(r, map[string]string{"id": "7"})
			w := httptest.NewRecorder()
			h.Plan(w, r)
			if w.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantInMsg != "" && !strings.Contains(w.Body.String(), tt.wantInMsg) {
				t.Errorf("body %s, want %q", w.Body.String(), tt.wantInMsg)
			}
			if tt.wantStatus == 409 && tt.planErr == nil && called {
				t.Error("an undeclared server reached Modrinth")
			}
			if tt.wantStatus != 200 {
				return
			}
			if want := map[string]string{"OK": "ok1", "BUSY": "busy1"}; !reflect.DeepEqual(gotInstalled, want) {
				t.Errorf("installed = %v, want %v", gotInstalled, want)
			}
			var body struct {
				Success      bool                  `json:"success"`
				Install      []services.PlanEntry  `json:"install"`
				Unresolved   []services.DepProblem `json:"unresolved"`
				Incompatible []services.DepProblem `json:"incompatible"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !body.Success || len(body.Install) != 1 || body.Unresolved == nil || body.Incompatible == nil {
				t.Errorf("body = %s", w.Body.String())
			}
		})
	}
}
