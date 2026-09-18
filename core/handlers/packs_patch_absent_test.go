package handlers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// The pack PATCH body used plain (non-pointer) fields, so a body that simply
// left a field out decoded to the zero value and wrote it over the stored one.

type packPatchFakeStore struct {
	store.Store
	pack    *models.Pack
	updated *models.Pack
}

func (f *packPatchFakeStore) GetPack(id int) (*models.Pack, error) {
	if f.pack != nil && f.pack.ID == id {
		cp := *f.pack
		return &cp, nil
	}
	return nil, errors.New("not found")
}

func (f *packPatchFakeStore) UpdatePack(p *models.Pack) error {
	cp := *p
	f.updated = &cp
	return nil
}

func newPackPatchHandler() (*PacksHandler, *packPatchFakeStore) {
	fs := &packPatchFakeStore{pack: &models.Pack{
		ID:           1,
		OwnerID:      "owner-id",
		InternalName: "My Pack",
		InternalSlug: "my-pack",
		Summary:      "a summary",
	}}
	return NewPacksHandler(&AppState{Store: fs}), fs
}

func packPatchRequest(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader([]byte(body)))
	r = mux.SetURLVars(r, map[string]string{"id": "1"})
	ctx := context.WithValue(r.Context(), "userID", "owner-id")
	ctx = context.WithValue(ctx, "isAdmin", false)
	ctx = context.WithValue(ctx, "username", "owner")
	return r.WithContext(ctx)
}

func TestUpdatePack_AbsentFieldsAreLeftAlone(t *testing.T) {
	h, fs := newPackPatchHandler()
	rec := httptest.NewRecorder()

	h.Update(rec, packPatchRequest("/api/packs/1", `{"name":"Renamed"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if fs.updated == nil {
		t.Fatal("nothing was persisted")
	}
	if fs.updated.InternalName != "Renamed" {
		t.Errorf("name = %q, want the submitted one", fs.updated.InternalName)
	}
	if fs.updated.Summary != "a summary" {
		t.Errorf("summary = %q, want the stored value", fs.updated.Summary)
	}
}

// Summary is the field an empty-string PATCH legitimately clears, so pin that
// "" and absent are now genuinely different.
func TestUpdatePack_ExplicitEmptySummaryStillClears(t *testing.T) {
	h, fs := newPackPatchHandler()
	rec := httptest.NewRecorder()

	h.Update(rec, packPatchRequest("/api/packs/1", `{"summary":""}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if fs.updated == nil || fs.updated.Summary != "" {
		t.Fatalf("summary = %v, want cleared", fs.updated)
	}
}
