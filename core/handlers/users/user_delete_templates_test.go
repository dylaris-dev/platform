package users

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"

	"github.com/gorilla/mux"
)

// The template rows cascade with the account; their files do not, so the
// delete has to remove them or they stay in storage with nothing pointing at them.
type userDeleteTemplatesStore struct {
	userDeleteWarpStore
	settings map[string]string
	tpl      models.Template
	deleted  bool
}

func (f *userDeleteTemplatesStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *userDeleteTemplatesStore) ListTemplateIDsByOwner(string) ([]int, error) {
	return []int{f.tpl.ID}, nil
}
func (f *userDeleteTemplatesStore) GetTemplate(int) (*models.Template, error) {
	if f.deleted {
		return nil, nil
	}
	t := f.tpl
	return &t, nil
}
func (f *userDeleteTemplatesStore) DeleteUser(string) error { f.deleted = true; return nil }

func TestDeleteUserRemovesTheirTemplateFiles(t *testing.T) {
	const victimID = "3f1c2b8e-0d44-4a91-9a6f-1d2e3c4b5a60"
	dir := t.TempDir()
	fs := &userDeleteTemplatesStore{
		settings: map[string]string{
			"core_storage_backend":        "path",
			"core_storage_path":           filepath.ToSlash(strings.TrimPrefix(dir, filepath.VolumeName(dir))),
			"core_storage_path_confirmed": "true",
		},
		tpl: models.Template{ID: 7, OwnerID: victimID, Name: "base", Kind: "server", Visibility: "personal"},
	}
	state := withCoreStorage(&handlers.AppState{Store: fs})
	ctx := context.Background()
	if err := state.CoreStorage.TemplateWrite(ctx, &fs.tpl, "server.properties", strings.NewReader("motd=x"), 6); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodDelete, "/api/users/"+victimID, strings.NewReader(`{"reauth":{"password":"`+handlertest.ReauthPassword+`"}}`))
	rctx := context.WithValue(r.Context(), "userID", "admin-id")
	rctx = context.WithValue(rctx, "username", "admin")
	rctx = context.WithValue(rctx, "isAdmin", true)
	r = mux.SetURLVars(r.WithContext(rctx), map[string]string{"id": victimID})
	rec := httptest.NewRecorder()
	NewUserHandler(state).DeleteUser(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	files, err := state.CoreStorage.TemplateList(ctx, &fs.tpl)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("the deleted account's template files are still stored: %v", files)
	}
}
