package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-core/store"
)

// libraryDenylistFakeStore serves the core-storage settings the library
// handlers need to build a provider, plus a scriptable denylist read.
type libraryDenylistFakeStore struct {
	store.Store
	values   map[string]string
	disabled []string
	listErr  error
}

func (f *libraryDenylistFakeStore) GetSetting(key string) (string, error) {
	return f.values[key], nil
}

func (f *libraryDenylistFakeStore) ListDisabledLibraryPaths() ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.disabled, nil
}

// newLibraryTestState seeds a path-backed library containing one file and
// returns the handler state pointing at it.
func newLibraryTestState(t *testing.T, fake *libraryDenylistFakeStore) *AppState {
	t.Helper()
	dir := testConnectionProbeDir(t)
	libDir := filepath.Join(dir, CoreStoragePrefixLibrary, "secret-mods")
	if err := os.MkdirAll(libDir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", libDir, err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "hidden.jar"), []byte("payload"), 0644); err != nil {
		t.Fatalf("seed hidden.jar: %v", err)
	}
	fake.values = map[string]string{
		keyCoreStorageBackend:     "path",
		keyCoreStoragePath:        dir,
		keyCoreStoragePathConfirm: "true",
	}
	return &AppState{Store: fake}
}

func nonAdminRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	return req.WithContext(context.WithValue(req.Context(), "isAdmin", false))
}

// TestDownloadLibrary_FailsClosedWhenDenylistUnreadable is the security half of
// the fix. disabledPathSet used to swallow the store error and return an empty
// set, and an empty set means "nothing is disabled" - so a transient DB fault
// handed a non-admin every file an admin had explicitly turned off. 503 mirrors
// how AuthMiddleware distinguishes a DB fault from a real authorization answer.
func TestDownloadLibrary_FailsClosedWhenDenylistUnreadable(t *testing.T) {
	fake := &libraryDenylistFakeStore{listErr: errors.New("connection refused")}
	h := &LibraryHandler{state: newLibraryTestState(t, fake)}

	rw := httptest.NewRecorder()
	h.DownloadLibraryHandler(rw, nonAdminRequest(http.MethodGet, "/api/library/download?path=secret-mods/hidden.jar"))

	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; a failed denylist read must not serve the file (body: %s)",
			rw.Code, rw.Body.String())
	}
	if strings.Contains(rw.Body.String(), "payload") {
		t.Error("response body contains the file content; the download was served despite the failed check")
	}
}

// TestDownloadLibrary_StillBlocksDisabledPath is the control: with a readable
// denylist the handler must keep returning 403, not the new 503.
func TestDownloadLibrary_StillBlocksDisabledPath(t *testing.T) {
	fake := &libraryDenylistFakeStore{disabled: []string{"secret-mods"}}
	h := &LibraryHandler{state: newLibraryTestState(t, fake)}

	rw := httptest.NewRecorder()
	h.DownloadLibraryHandler(rw, nonAdminRequest(http.MethodGet, "/api/library/download?path=secret-mods/hidden.jar"))

	if rw.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (ancestor path is disabled) (body: %s)", rw.Code, rw.Body.String())
	}
}

// TestDownloadLibrary_AllowsEnabledPath is the other control: a readable,
// empty denylist must still let the download through.
func TestDownloadLibrary_AllowsEnabledPath(t *testing.T) {
	fake := &libraryDenylistFakeStore{}
	h := &LibraryHandler{state: newLibraryTestState(t, fake)}

	rw := httptest.NewRecorder()
	h.DownloadLibraryHandler(rw, nonAdminRequest(http.MethodGet, "/api/library/download?path=secret-mods/hidden.jar"))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (nothing is disabled) (body: %s)", rw.Code, rw.Body.String())
	}
	if got := rw.Body.String(); got != "payload" {
		t.Errorf("body = %q, want %q", got, "payload")
	}
}

// TestGetLibrary_FailsClosedWhenDenylistUnreadable covers the listing handler,
// which filters disabled children out of the response using the same set.
func TestGetLibrary_FailsClosedWhenDenylistUnreadable(t *testing.T) {
	fake := &libraryDenylistFakeStore{listErr: errors.New("connection refused")}
	h := &LibraryHandler{state: newLibraryTestState(t, fake)}

	rw := httptest.NewRecorder()
	h.GetLibraryHandler(rw, nonAdminRequest(http.MethodGet, "/api/library?path=/"))

	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", rw.Code, rw.Body.String())
	}
	if strings.Contains(rw.Body.String(), "secret-mods") {
		t.Error("listing leaked the disabled directory name after a failed denylist read")
	}
}

// The storage backends clean a path themselves, so "./secret-mods/hidden.jar"
// found the disabled file while the denylist walked a different string. Browse
// and download now take the one spelling the denylist compares against, like
// the mirror always did - and a plain path still works.
func TestADisabledLibraryPathCannotBeReachedByAnotherSpelling(t *testing.T) {
	fake := &libraryDenylistFakeStore{disabled: []string{"secret-mods"}}
	h := &LibraryHandler{state: newLibraryTestState(t, fake)}

	for _, p := range []string{"./secret-mods/hidden.jar", "secret-mods/./hidden.jar", "secret-mods//hidden.jar"} {
		rw := httptest.NewRecorder()
		h.DownloadLibraryHandler(rw, nonAdminRequest(http.MethodGet, "/api/library/download?path="+p))
		if strings.Contains(rw.Body.String(), "payload") || rw.Code == http.StatusOK {
			t.Fatalf("download %q: status %d, the disabled file was served", p, rw.Code)
		}
	}
	rw := httptest.NewRecorder()
	h.GetLibraryHandler(rw, nonAdminRequest(http.MethodGet, "/api/library?path=./secret-mods"))
	if strings.Contains(rw.Body.String(), "hidden.jar") {
		t.Fatalf("the disabled folder was listed: %s", rw.Body.String())
	}

	// The ordinary spelling, with a leading slash as the panel sends it.
	fake.disabled = nil
	rw = httptest.NewRecorder()
	h.DownloadLibraryHandler(rw, nonAdminRequest(http.MethodGet, "/api/library/download?path=/secret-mods/hidden.jar"))
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), "payload") {
		t.Fatalf("an enabled file is no longer served: %d %s", rw.Code, rw.Body.String())
	}
	rw = httptest.NewRecorder()
	h.GetLibraryHandler(rw, nonAdminRequest(http.MethodGet, "/api/library?path=/secret-mods"))
	if !strings.Contains(rw.Body.String(), "hidden.jar") {
		t.Fatalf("an enabled folder is no longer listed: %s", rw.Body.String())
	}
}
