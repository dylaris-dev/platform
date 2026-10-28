package packs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFrozenBuildDeletionDoesNotGrantDeleteRights(t *testing.T) {
	h, fs, _ := newPackCleanup(t)
	fs.builds[0].Frozen = true
	h.state.Store = &publishBindingStore{packCleanupStore: fs}
	r := packReq("DELETE", map[string]string{"id": "1", "buildId": "10"}, `{"confirmVersion":"1.0"}`)
	r = r.WithContext(context.WithValue(r.Context(), "userID", "editor"))
	w := httptest.NewRecorder()
	h.DeleteBuild(w, r)
	if w.Code != http.StatusForbidden || len(fs.deleted) != 0 {
		t.Fatalf("an editor without delete rights could delete the build: %d", w.Code)
	}
}

func TestFrozenBuildDeletionRequiresExactConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"missing", "", http.StatusConflict},
		{"wrong", `{"confirmVersion":"another-version"}`, http.StatusConflict},
		{"confirmed", `{"confirmVersion":"1.0"}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fs, root := newPackCleanup(t)
			fs.builds[0].Frozen = true
			fs.builds[0].ModrinthPublished = true
			fs.builds[0].VersionString = "1.0"
			gone := seedBuild(t, h, root, &fs.builds[0])
			kept := seedBuild(t, h, root, &fs.builds[1])
			w := httptest.NewRecorder()
			h.DeleteBuild(w, packReq("DELETE", map[string]string{"id": "1", "buildId": "10"}, tc.body))
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d: %s", w.Code, tc.want, w.Body.String())
			}
			deleted := len(fs.deleted) > 0
			if deleted != (tc.want == http.StatusOK) || exists(root, gone+"pack.mrpack") == deleted {
				t.Fatal("confirmation and artifact deletion did not agree")
			}
			if !exists(root, kept+"pack.mrpack") {
				t.Fatal("sibling build lost its artifact")
			}
		})
	}
}

func TestPublishedBuildMetadataAndRepublishAreReadOnly(t *testing.T) {
	h, fs, _ := newPackCleanup(t)
	fs.builds[0].Frozen = true
	fs.builds[0].ModrinthPublished = true
	h.SetPATLoader(&publishBindingPAT{})
	for _, action := range []string{"metadata", "publish"} {
		w := httptest.NewRecorder()
		r := packReq("PATCH", map[string]string{"id": "1", "buildId": "10"}, `{"changelog":"changed","channel":"release"}`)
		if action == "publish" {
			h.PublishModrinth(w, r)
		} else {
			h.UpdateBuild(w, r)
		}
		if w.Code != http.StatusConflict {
			t.Fatalf("%s status=%d: %s", action, w.Code, w.Body.String())
		}
	}
}
