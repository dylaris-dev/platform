package packs

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

type serverExportStore struct {
	*draftRenderStore
	pack  *models.Pack
	build *models.PackBuild
}

func (s *serverExportStore) GetPack(int) (*models.Pack, error)           { return s.pack, nil }
func (s *serverExportStore) GetPackBuild(int) (*models.PackBuild, error) { return s.build, nil }

// The pack is bound to "proj": "u" manages it, "reader" is a plain member.
func (s *serverExportStore) GetModrinthMembership(user, project string) (*models.ModrinthMembership, error) {
	switch user {
	case "u":
		return member(services.ModrinthPermAll), nil
	case "reader":
		return member(0), nil
	}
	return nil, nil
}
func (s *serverExportStore) UpsertModrinthMembership(*models.ModrinthMembership) error { return nil }

func TestAuthenticatedServerExport(t *testing.T) {
	h, st, _, pack, build := serverPackFixture(t)
	pack.ModrinthProjectID = "proj"
	h.state.Store = &serverExportStore{draftRenderStore: st, pack: pack, build: build}
	request := func(user, kind string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/packs/1/builds/1/export?kind="+kind, nil)
		r = r.WithContext(context.WithValue(r.Context(), "userID", user))
		r = mux.SetURLVars(r, map[string]string{"id": "1", "buildId": "1"})
		w := httptest.NewRecorder()
		h.ExportMrpack(w, r)
		return w
	}

	for _, user := range []string{"u", "reader"} {
		w := request(user, "server")
		if w.Code != http.StatusOK {
			t.Fatalf("%s export: %d %s", user, w.Code, w.Body.String())
		}
		zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
		if err != nil || len(zr.File) != 1 || zr.File[0].Name != "mods/test.jar" {
			t.Fatalf("invalid server ZIP: %v", err)
		}
		if w.Header().Get("Content-Type") != "application/zip" || !strings.Contains(w.Header().Get("Content-Disposition"), "p-1.0-server.zip") {
			t.Fatal("missing ZIP download headers")
		}
	}
	if w := request("stranger", "server"); w.Code != http.StatusForbidden {
		t.Fatalf("uninvited user export: status %d", w.Code)
	}
	build.PackID = 2
	if w := request("u", "server"); w.Code != http.StatusNotFound {
		t.Fatalf("foreign build export: status %d", w.Code)
	}
	build.PackID = 1
	if w := request("u", "unknown"); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown download kind: status %d", w.Code)
	}
	if w := request("u", "client"); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/x-modrinth-modpack+zip" {
		t.Fatalf("client export regressed: %d %s", w.Code, w.Body.String())
	}
	if build.Frozen || build.MrpackStorageKey != "" {
		t.Fatal("downloading mutated the draft build")
	}
}
