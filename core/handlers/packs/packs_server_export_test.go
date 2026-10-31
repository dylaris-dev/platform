package packs

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// The pack is bound to "proj": "u" manages it, "editor" may upload versions,
// "reader" is a plain member, "lapsed" had full rights until the token was
// rejected an hour ago, and "admin" is a platform admin outside the team.
func (s *serverExportStore) GetModrinthMembership(user, project string) (*models.ModrinthMembership, error) {
	switch user {
	case "u":
		return member(services.ModrinthPermAll), nil
	case "editor":
		return member(services.ModrinthPermUploadVersion), nil
	case "reader":
		return member(0), nil
	case "lapsed":
		return lapsed(time.Hour), nil
	}
	return nil, nil
}
func (s *serverExportStore) UpsertModrinthMembership(*models.ModrinthMembership) error { return nil }

// The server ZIP embeds Modrinth's jars, so only the owner and members who may
// edit the pack get one; the .mrpack only references them and stays readable.
func TestServerExportNeedsEditAccess(t *testing.T) {
	h, st, _, pack, build := serverPackFixture(t)
	pack.ModrinthProjectID = "proj"
	h.state.Store = &serverExportStore{draftRenderStore: st, pack: pack, build: build}
	request := func(user string, admin bool, kind string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/packs/1/builds/1/export?kind="+kind, nil)
		ctx := context.WithValue(r.Context(), "userID", user)
		ctx = context.WithValue(ctx, "isAdmin", admin)
		r = mux.SetURLVars(r.WithContext(ctx), map[string]string{"id": "1", "buildId": "1"})
		w := httptest.NewRecorder()
		h.ExportMrpack(w, r)
		return w
	}

	for _, c := range []struct {
		name      string
		user      string
		admin     bool
		server    int
		clientMay bool
	}{
		{"owner", "u", false, http.StatusOK, true},
		{"edit member", "editor", false, http.StatusOK, true},
		{"read-only member", "reader", false, http.StatusForbidden, true},
		{"lapsed token", "lapsed", false, http.StatusForbidden, true},
		{"admin outside the team", "admin", true, http.StatusForbidden, true},
		{"stranger", "stranger", false, http.StatusForbidden, false},
	} {
		w := request(c.user, c.admin, "server")
		if w.Code != c.server {
			t.Errorf("%s server export: %d %s, want %d", c.name, w.Code, w.Body.String(), c.server)
			continue
		}
		if c.server == http.StatusOK {
			zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
			if err != nil || len(zr.File) != 1 || zr.File[0].Name != "mods/test.jar" {
				t.Fatalf("%s: invalid server ZIP: %v", c.name, err)
			}
			if w.Header().Get("Content-Type") != "application/zip" || !strings.Contains(w.Header().Get("Content-Disposition"), "p-1.0-server.zip") {
				t.Fatalf("%s: missing ZIP download headers", c.name)
			}
		}
		if got := request(c.user, c.admin, "client").Code == http.StatusOK; got != c.clientMay {
			t.Errorf("%s client export allowed=%v, want %v", c.name, got, c.clientMay)
		}
	}

	build.PackID = 2
	if w := request("u", false, "server"); w.Code != http.StatusNotFound {
		t.Fatalf("foreign build export: status %d", w.Code)
	}
	build.PackID = 1
	if w := request("u", false, "unknown"); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown download kind: status %d", w.Code)
	}
	if build.Frozen || build.MrpackStorageKey != "" {
		t.Fatal("downloading mutated the draft build")
	}
}
