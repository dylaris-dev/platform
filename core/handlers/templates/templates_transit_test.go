package templates

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
)

// unreachableOwnStorage is ownStorage with a bucket Core can never reach: the
// tenant dialer refuses loopback, so every file operation fails without a
// network round trip.
func unreachableOwnStorage(st *fakeStore) {
	ownStorage(st)
	st.storages[5].Config = json.RawMessage(`{"endpoint":"http://127.0.0.1:9","bucket":"b","accessKeyId":"AK","secretAccessKey":"SK","forcePathStyle":true}`)
}

// privateTemp points os.TempDir at a fresh dir, so a test can see every file a
// request spooled there (multipart parts included) and nothing of other tests.
func privateTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, dir)
	}
	return dir
}

func entriesIn(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func TestRequestBound(t *testing.T) {
	mib := func(n int64) *int64 { v := n << 20; return &v }
	tests := []struct {
		name string
		s    services.TemplateSettings
		own  bool
		want *int64
	}{
		{"our storage keeps the template cap", services.TemplateSettings{MaxSizeBytes: mib(512), MaxSizeBytesAdmin: mib(2048)}, false, mib(512)},
		{"our storage uncapped stays uncapped", services.TemplateSettings{MaxSizeBytesAdmin: mib(2048)}, false, nil},
		{"own storage takes the admin cap", services.TemplateSettings{MaxSizeBytesAdmin: mib(2048)}, true, mib(2048)},
		{"own storage, admin cap 0, takes the default", services.TemplateSettings{MaxSizeBytesAdmin: mib(0)}, true, mib(2048)},
		{"own storage, admin unlimited, falls to the hard body cap", services.TemplateSettings{}, true, nil},
	}
	for _, tc := range tests {
		got := requestBound(tc.s, tc.own)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if bodyLimit(nil) != maxUploadBody || bodyLimit(mib(1)) != 1<<20+uploadSlack {
		t.Errorf("bodyLimit: %d / %d", bodyLimit(nil), bodyLimit(mib(1)))
	}
}

// Own storage has no template size cap, but one request is still held to the
// transit ceiling (the admin cap) because it is spooled on Core's disk first.
func TestOwnStorageUploadIsBoundByTheTransitCeiling(t *testing.T) {
	settings := map[string]string{services.TemplatesMaxSizeMBKey: "0", services.TemplatesMaxSizeMBAdminKey: "1"}
	tests := []struct {
		name          string
		withoutLength bool
	}{
		{"declared length", false},
		{"streamed", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmp := privateTemp(t)
			h, st := newTestHandler(t, settings)
			ownStorage(st)
			// Over 1 MiB + slack: raw bytes, so the refusal is the body bound.
			r := uploadRequest(t, `{"name":"big","kind":"server"}`, bytes.Repeat([]byte("x"), 1<<20+uploadSlack+1))
			if tc.withoutLength {
				r.ContentLength = -1
			}
			rec := httptest.NewRecorder()
			h.Upload(rec, asUser(r, ownerID, false))
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("got %d (%s), want 413", rec.Code, rec.Body)
			}
			if len(st.templates) != 0 {
				t.Errorf("%d rows left", len(st.templates))
			}
			if left := entriesIn(t, tmp); len(left) != 0 {
				t.Errorf("temp files left: %v", left)
			}
		})
	}
}

// Under the ceiling the request is accepted although the user cap ("0") would
// refuse any byte. The unreachable bucket then fails the write, which shows
// the request got that far, and the failed create leaves no row (I2).
func TestOwnStorageUploadUnderTheCeilingPassesTheCaps(t *testing.T) {
	tmp := privateTemp(t)
	h, st := newTestHandler(t, map[string]string{services.TemplatesMaxSizeMBKey: "0", services.TemplatesMaxSizeMBAdminKey: "1"})
	unreachableOwnStorage(st)
	z := makeZip(t, []zipEntry{{name: "server.properties", body: []byte("motd=hi")}})
	rec := httptest.NewRecorder()
	h.Upload(rec, asUser(uploadRequest(t, `{"name":"up","kind":"server"}`, z), ownerID, false))
	if rec.Code == http.StatusRequestEntityTooLarge || rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d (%s), want the storage write to be what fails (500)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Could not store the template files") {
		t.Errorf("unexpected answer %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), ownFilesMayRemain) {
		t.Errorf("the answer does not say files may remain: %s", rec.Body)
	}
	if len(st.templates) != 0 {
		t.Errorf("the discarded own-storage template kept its row")
	}
	if left := entriesIn(t, tmp); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestOwnStorageAddFilesIsBoundByTheTransitCeiling(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		dir := withSmallBundleCap(t, bundleBytesCap)
		h, st := newTestHandler(t, map[string]string{services.TemplatesMaxSizeMBKey: "0", services.TemplatesMaxSizeMBAdminKey: "1"})
		ownStorage(st)
		tpl := seedTemplate(t, st, ownerID, "personal")
		id := 5
		st.templates[tpl.ID].BackupStorageID = &id
		r := addRequest(t, []part{{"path", "", "world.bin"}, {"file", "world.bin", strings.Repeat("x", 1<<20+uploadSlack+1)}})
		if streamed {
			r.ContentLength = -1
		}
		rec := httptest.NewRecorder()
		h.AddFiles(rec, withID(asUser(r, ownerID, false), tpl.ID))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("streamed=%v: got %d (%s), want 413", streamed, rec.Code, rec.Body)
		}
		if n := stagedLeft(t, dir); n != 0 {
			t.Errorf("streamed=%v: %d staged files left", streamed, n)
		}
	}
}

// A template on an unreachable tenant bucket can still be deleted: the row
// goes and the user is told files may remain. On OUR storage a failed file
// delete keeps the row, so it can be retried and nothing is orphaned.
func TestDeleteWhenFilesCannotBeRemoved(t *testing.T) {
	for _, own := range []bool{true, false} {
		h, st := newTestHandler(t, nil)
		tpl := seedTemplate(t, st, ownerID, "personal")
		if own {
			unreachableOwnStorage(st)
			id := 5
			st.templates[tpl.ID].BackupStorageID = &id
		} else {
			// The fake store cannot load any storage connection.
			st.templates[tpl.ID].StorageConnectionID = 9
		}
		rec := httptest.NewRecorder()
		h.Delete(rec, withID(asUser(httptest.NewRequest("DELETE", "/x", nil), ownerID, false), tpl.ID))
		_, kept := st.templates[tpl.ID]
		if own {
			if rec.Code != http.StatusOK || kept || !strings.Contains(rec.Body.String(), ownFilesMayRemain) {
				t.Errorf("own: got %d (%s), row kept %v; want 200, gone, with the warning", rec.Code, rec.Body, kept)
			}
		} else if rec.Code != http.StatusInternalServerError || !kept {
			t.Errorf("ours: got %d (%s), row kept %v; want 500 and the row kept", rec.Code, rec.Body, kept)
		}
	}
}

// A template created on own storage is not stamped with the operator's
// template connection: it would count as a reference to that connection.
func TestOwnStorageTemplateIsNotStampedWithAConnection(t *testing.T) {
	h, st := newTestHandler(t, map[string]string{services.TemplatesStorageConnectionKey: "3"})
	ownStorage(st)
	rec := httptest.NewRecorder()
	h.Create(rec, asUser(httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"n","kind":"server"}`)), ownerID, false))
	if rec.Code != http.StatusCreated || len(st.templates) != 1 {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body)
	}
	for _, tpl := range st.templates {
		if tpl.StorageConnectionID != 0 || tpl.BackupStorageID == nil {
			t.Errorf("stored connection %d, storage %v; want 0 and 5", tpl.StorageConnectionID, tpl.BackupStorageID)
		}
	}
}

// Each listed template carries its own cap: the user cap, the admin cap for
// admin-level, platform and admin-owned ones, none on own storage.
func TestListGivesEachTemplateItsCap(t *testing.T) {
	h, st := newTestHandler(t, map[string]string{services.TemplatesMaxSizeMBKey: "10", services.TemplatesMaxSizeMBAdminKey: "20", services.TemplatesPlatformForUsersKey: "true"})
	st.users["admin"] = &models.User{ID: adminID, Username: "admin", IsAdmin: true}
	ownStorage(st)
	mine := seedTemplate(t, st, ownerID, "personal")
	onOwn := seedTemplate(t, st, ownerID, "personal")
	id := 5
	st.templates[onOwn.ID].BackupStorageID = &id
	platform := seedTemplate(t, st, ownerID, "platform")
	adminOwned := seedTemplate(t, st, adminID, "personal")
	st.shares[adminOwned.ID] = map[string]string{ownerID: "use"}

	rec := httptest.NewRecorder()
	h.List(rec, asUser(httptest.NewRequest("GET", "/x", nil), ownerID, false))
	var resp struct {
		Templates []struct {
			ID           int    `json:"id"`
			MaxSizeBytes *int64 `json:"maxSizeBytes"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := map[int]int64{mine.ID: 10 << 20, onOwn.ID: -1, platform.ID: 20 << 20, adminOwned.ID: 20 << 20}
	if len(resp.Templates) != len(want) {
		t.Fatalf("listed %d templates: %s", len(resp.Templates), rec.Body)
	}
	for _, it := range resp.Templates {
		got := int64(-1)
		if it.MaxSizeBytes != nil {
			got = *it.MaxSizeBytes
		}
		if got != want[it.ID] {
			t.Errorf("template %d: cap %d, want %d", it.ID, got, want[it.ID])
		}
	}

	rec = httptest.NewRecorder()
	h.Get(rec, withID(asUser(httptest.NewRequest("GET", "/x", nil), ownerID, false), onOwn.ID))
	if !strings.Contains(rec.Body.String(), `"maxSizeBytes":null`) {
		t.Errorf("own-storage Get: %s", rec.Body)
	}
}

// On own storage the DECOMPRESSED bytes of one extraction are held to the
// transit ceiling too: a small zip whose nested archive inflates past it is
// refused while it is being staged, before anything reaches the bucket, and
// leaves nothing in the temp dir. The answer says files may remain, because
// the unreachable bucket could not be cleaned.
func TestOwnStorageExtractionIsBoundByTheTransitCeiling(t *testing.T) {
	tmp := privateTemp(t)
	h, st := newTestHandler(t, map[string]string{services.TemplatesMaxSizeMBKey: "0", services.TemplatesMaxSizeMBAdminKey: "1"})
	unreachableOwnStorage(st)
	z := makeZip(t, []zipEntry{{name: "worlds/big.zip", body: make([]byte, 2<<20)}})
	if len(z) > 1<<20 {
		t.Fatalf("test zip is %d bytes; it must be small to test the decompressed bound", len(z))
	}
	rec := httptest.NewRecorder()
	h.Upload(rec, asUser(uploadRequest(t, `{"name":"bomb","kind":"server"}`, z), ownerID, false))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d (%s), want 413", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), ownFilesMayRemain) {
		t.Errorf("the answer does not say files may remain: %s", rec.Body)
	}
	if len(st.templates) != 0 {
		t.Errorf("%d rows left", len(st.templates))
	}
	if left := entriesIn(t, tmp); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}
