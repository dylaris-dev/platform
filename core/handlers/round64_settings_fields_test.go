package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

func settingsFieldsServer(id int, role string) models.Server {
	return models.Server{ID: id, Role: role, OwnerID: "owner-1",
		StartCommand: "java -Dapi.token=s3cret -jar server.jar", ExtraJvmFlags: "-Dapi.token=s3cret",
		Cpuset: "2-3", CPUPinningMode: "manual"}
}

func settingsFieldsBlank(s models.Server) bool {
	return s.StartCommand == "" && s.ExtraJvmFlags == "" && s.Cpuset == "" && s.CPUPinningMode == ""
}

// A member sees the JVM flags, start command and cpuset only with the right to
// change the settings; the owner's own rows are untouched.
func TestServerListBlanksSettingsFieldsWithoutTheSettingsCap(t *testing.T) {
	st := tabListAuthzStore{grants: map[int]*store.ServerGrant{
		1: {CapOverrides: store.CapOverrides{Grant: []string{"console.read"}}},
		2: {CapOverrides: store.CapOverrides{Grant: []string{"console.read", "server.settings.write"}}},
	}}
	state := &AppState{Authz: authz.NewResolver(st)}
	got := applyResolvedTabPermissions(state, []models.Server{
		settingsFieldsServer(1, "invited"), settingsFieldsServer(2, "invited"), settingsFieldsServer(3, "owner"),
	}, "friend-1", "friend")
	byID := map[int]models.Server{}
	for _, s := range got {
		byID[s.ID] = s
	}
	if !settingsFieldsBlank(byID[1]) {
		t.Errorf("a console-only member got the settings fields: %+v", byID[1])
	}
	if byID[2].ExtraJvmFlags == "" || byID[2].StartCommand == "" || byID[2].Cpuset == "" {
		t.Errorf("a member with server.settings.write lost the settings fields: %+v", byID[2])
	}
	if byID[3].ExtraJvmFlags == "" {
		t.Error("the owner's own row was redacted")
	}
}

// externalServerStore serves one server owned by owner-1 and the key owners.
type externalServerStore struct {
	store.Store
}

func (externalServerStore) GetServerByUUID(string) (*models.Server, error) {
	s := settingsFieldsServer(1, "")
	return &s, nil
}
func (externalServerStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, IsAdmin: id == "admin-1"}, nil
}
func (externalServerStore) GetSetting(string) (string, error) { return "", nil }

// The external single-server route applies the same rule to the key's owner.
func TestExternalServerBlanksSettingsFieldsWithoutTheSettingsCap(t *testing.T) {
	resolver := authz.NewResolver(tabListAuthzStore{grants: map[int]*store.ServerGrant{
		1: {CapOverrides: store.CapOverrides{Grant: []string{"overview.read", "console.read"}}},
	}})
	get := func(caller string) models.Server {
		t.Helper()
		h := &APIKeysHandler{state: &AppState{Store: externalServerStore{}, Authz: resolver}}
		r := httptest.NewRequest("GET", "/api/external/servers/u", nil)
		r = mux.SetURLVars(r, map[string]string{"uuid": "u"})
		r = r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey{}, &apiKeyCtx{key: &models.APIKey{ID: 1, UserID: caller}}))
		rec := httptest.NewRecorder()
		h.GetExternalServer(rec, r)
		var out struct{ Server models.Server }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		return out.Server
	}
	if s := get("friend-1"); !settingsFieldsBlank(s) {
		t.Errorf("a console-only member's key got the settings fields: %+v", s)
	}
	for _, caller := range []string{"owner-1", "admin-1"} {
		if s := get(caller); s.ExtraJvmFlags == "" || s.StartCommand == "" {
			t.Errorf("%s lost the settings fields", caller)
		}
	}
}

// Demo rows are always shown without the settings fields: the demo account
// never holds the right to change settings.
func TestDemoRowsCarryNoSettingsFields(t *testing.T) {
	if body := funcBody(t, "servers.go", "GetServers"); !regexp.MustCompile(`ds.Permissions = &models.TabPermissions{Console: true, Files: true}\s+redactSettingsFields\(ds\)`).MatchString(body) {
		t.Error("demo rows are appended without redactSettingsFields")
	}
}

// An admin invited onto a server on a customer's machine is a guest there: the
// settings fields follow the owner's grant, on the list and the external route.
func TestAdminOnACustomersMachineSeesSettingsFieldsOnlyWhenGranted(t *testing.T) {
	resolver := authz.NewResolver(tabListAuthzStore{grants: map[int]*store.ServerGrant{
		1: {CapOverrides: store.CapOverrides{Grant: []string{"overview.read", "console.read"}}},
	}})
	resolver.SetForeignNode(func(int, string) bool { return true })
	state := &AppState{Authz: resolver}

	byon := settingsFieldsServer(1, "admin")
	byon.NodeKind = models.NodeKindBYON
	platform := settingsFieldsServer(2, "admin")
	platform.NodeKind = models.NodeKindPlatform
	got := applyResolvedTabPermissions(state, []models.Server{byon, platform}, "admin-1", "admin")
	if len(got) != 2 {
		t.Fatalf("admin rows dropped: %d", len(got))
	}
	if !settingsFieldsBlank(got[0]) {
		t.Errorf("an admin with console only on a customer's machine got the settings fields: %+v", got[0])
	}
	if got[1].ExtraJvmFlags == "" {
		t.Error("an admin's row on a platform node was redacted")
	}

	h := &APIKeysHandler{state: &AppState{Store: externalServerStore{}, Authz: resolver}}
	r := httptest.NewRequest("GET", "/api/external/servers/u", nil)
	r = mux.SetURLVars(r, map[string]string{"uuid": "u"})
	r = r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey{}, &apiKeyCtx{key: &models.APIKey{ID: 1, UserID: "admin-1"}}))
	rec := httptest.NewRecorder()
	h.GetExternalServer(rec, r)
	var out struct{ Server models.Server }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !settingsFieldsBlank(out.Server) {
		t.Errorf("external route gave an admin on a customer's machine the settings fields: %s", rec.Body.String())
	}
}

// adminListStore serves the admin list: one row on a customer's machine.
type adminListStore struct {
	store.Store
}

func (adminListStore) ListServersForUser(string, bool) ([]models.Server, error) {
	s := settingsFieldsServer(1, "admin")
	s.NodeKind = models.NodeKindBYON
	return []models.Server{s}, nil
}
func (adminListStore) CountInvitesPerServer() (map[int]int, error) { return nil, nil }

// The admin server list applies the same rule to an admin's row on a
// customer's machine.
func TestAdminServerListBlanksSettingsFieldsOnACustomersMachine(t *testing.T) {
	resolver := authz.NewResolver(tabListAuthzStore{grants: map[int]*store.ServerGrant{
		1: {CapOverrides: store.CapOverrides{Grant: []string{"console.read"}}},
	}})
	resolver.SetForeignNode(func(int, string) bool { return true })
	h := &ServerHandler{state: &AppState{Store: adminListStore{}, Authz: resolver}}
	r := httptest.NewRequest("GET", "/api/admin/servers", nil)
	ctx := context.WithValue(r.Context(), "userID", "admin-1")
	r = r.WithContext(context.WithValue(ctx, "isAdmin", true))
	rec := httptest.NewRecorder()
	h.GetAdminServers(rec, r)
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("admin list carries the settings fields of a customer's server: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":1`) {
		t.Fatalf("row missing: %d %s", rec.Code, rec.Body.String())
	}
}
