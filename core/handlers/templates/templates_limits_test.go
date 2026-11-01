package templates

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
)

const adminID = "44444444-4444-4444-4444-444444444444"

// ownStorage gives the owner tenant s3 storage 5 and makes it their templates
// target. Building the provider makes no network call.
func ownStorage(st *fakeStore) {
	owner := ownerID
	st.storages = map[int]*models.BackupStorage{5: {ID: 5, Name: "mine", Provider: "s3", OwnerID: &owner,
		Config: json.RawMessage(`{"endpoint":"https://s3.example.test","bucket":"b","accessKeyId":"AK","secretAccessKey":"SK"}`)}}
	st.prefs = map[string]int{ownerID: 5}
}

// The cap follows the template's OWNER and visibility, not who is editing it.
func TestSizeCapByVisibilityAndOwner(t *testing.T) {
	tests := []struct {
		name       string
		owner      string
		visibility string
		caller     string
		admin      bool
		want       int
	}{
		{"user personal is user-capped", ownerID, "personal", ownerID, false, http.StatusRequestEntityTooLarge},
		{"admin editing a user's personal is still user-capped", ownerID, "personal", adminID, true, http.StatusRequestEntityTooLarge},
		{"admin-owned personal gets the admin cap", adminID, "personal", adminID, true, http.StatusOK},
		{"platform template gets the admin cap", ownerID, "platform", adminID, true, http.StatusOK},
		{"admin-level template gets the admin cap", ownerID, "admin", adminID, true, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, st := newTestHandler(t, map[string]string{services.TemplatesMaxSizeMBKey: "0", services.TemplatesMaxSizeMBAdminKey: "1"})
			st.users["admin"] = &models.User{ID: adminID, Username: "admin", IsAdmin: true}
			tpl := seedTemplate(t, st, tc.owner, tc.visibility)
			rec := httptest.NewRecorder()
			body := `{"path":"server.properties","content":"motd=hi"}`
			h.PutText(rec, withID(asUser(httptest.NewRequest("PUT", "/x", strings.NewReader(body)), tc.caller, tc.admin), tpl.ID))
			if rec.Code != tc.want {
				t.Errorf("got %d (%s), want %d", rec.Code, rec.Body, tc.want)
			}
		})
	}
}

// On their own storage a user has neither a count nor a size limit, and those
// templates do not count against the limit on Dylaris storage.
func TestOwnStorageHasNoLimits(t *testing.T) {
	h, st := newTestHandler(t, map[string]string{services.TemplatesMaxPerUserKey: "1", services.TemplatesMaxSizeMBKey: "0"})
	ownStorage(st)
	create := func() (int, models.Template) {
		rec := httptest.NewRecorder()
		h.Create(rec, asUser(httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"n","kind":"server"}`)), ownerID, false))
		var tpl models.Template
		_ = json.Unmarshal(rec.Body.Bytes(), &tpl)
		return rec.Code, tpl
	}
	for i := 0; i < 3; i++ {
		code, tpl := create()
		if code != http.StatusCreated || tpl.BackupStorageID == nil || *tpl.BackupStorageID != 5 {
			t.Fatalf("own-storage create %d: got %d, storage %v", i+1, code, tpl.BackupStorageID)
		}
	}

	// The size cap of "0" would refuse any byte; own storage refuses none.
	r := withID(asUser(httptest.NewRequest("PUT", "/x", nil), ownerID, false), 1)
	tpl, _, s, ok := h.load(httptest.NewRecorder(), r, needEdit)
	if !ok || s.MaxSizeBytes != nil {
		t.Fatalf("own-storage template: ok %v cap %v, want no cap", ok, s.MaxSizeBytes)
	}
	if !h.fits(httptest.NewRecorder(), r, tpl, s, map[string]int64{"world.zip": 1 << 40}) {
		t.Error("fits refused a size on own storage")
	}
	// One capture still passes Core's disk: it is held to the transit ceiling.
	if b := captureBound(requestBound(s, true)); *b != 2048<<20+captureSlack {
		t.Errorf("capture bound %d, want the admin cap plus slack", *b)
	}

	// Back on Dylaris storage: the three own-storage templates do not count,
	// so one fits, and the second is over MaxPerUser.
	delete(st.prefs, ownerID)
	for i, want := range []int{http.StatusCreated, http.StatusConflict} {
		if code, tpl := create(); code != want || (code == http.StatusCreated && tpl.BackupStorageID != nil) {
			t.Fatalf("dylaris create %d: got %d storage %v, want %d", i+1, code, tpl.BackupStorageID, want)
		}
	}
	// And that template is still size-capped.
	rec := httptest.NewRecorder()
	h.PutText(rec, withID(asUser(httptest.NewRequest("PUT", "/x", strings.NewReader(`{"path":"a.yml","content":"a"}`)), ownerID, false), 4))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("dylaris-storage write: got %d (%s), want 413", rec.Code, rec.Body)
	}
}

// Admin-level and platform templates stay on platform storage whatever the
// creator's own preference is.
func TestAdminTemplateIgnoresOwnPreference(t *testing.T) {
	h, st := newTestHandler(t, nil)
	ownStorage(st)
	rec := httptest.NewRecorder()
	h.Create(rec, asUser(httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"n","kind":"server","visibility":"platform"}`)), ownerID, true))
	var tpl models.Template
	_ = json.Unmarshal(rec.Body.Bytes(), &tpl)
	if rec.Code != http.StatusCreated || tpl.BackupStorageID != nil {
		t.Fatalf("got %d storage %v, want 201 on platform storage", rec.Code, tpl.BackupStorageID)
	}
}

// A target that cannot be built refuses the create before a row exists, and
// never falls back to Core storage.
func TestUnusableOwnStorageIsRefused(t *testing.T) {
	h, st := newTestHandler(t, nil)
	ownStorage(st)
	st.storages[5].Config = json.RawMessage(`{"bucket":""}`)
	rec := httptest.NewRecorder()
	h.Create(rec, asUser(httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"n","kind":"server"}`)), ownerID, false))
	if rec.Code != http.StatusServiceUnavailable || len(st.templates) != 0 {
		t.Fatalf("got %d (%s), %d rows, want 503 and none", rec.Code, rec.Body, len(st.templates))
	}
}

func TestListReportsCapsAndTarget(t *testing.T) {
	for _, own := range []bool{false, true} {
		h, st := newTestHandler(t, nil)
		if own {
			ownStorage(st)
		}
		rec := httptest.NewRecorder()
		h.List(rec, asUser(httptest.NewRequest("GET", "/x", nil), ownerID, false))
		var resp struct {
			MaxSizeBytes      *int64 `json:"maxSizeBytes"`
			MaxSizeBytesAdmin *int64 `json:"maxSizeBytesAdmin"`
			OwnStorage        bool   `json:"ownStorage"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.MaxSizeBytes == nil || *resp.MaxSizeBytes != 512<<20 || resp.MaxSizeBytesAdmin == nil || *resp.MaxSizeBytesAdmin != 2048<<20 || resp.OwnStorage != own {
			t.Errorf("own=%v: %s", own, rec.Body)
		}
	}
}

// An own-storage template cannot become admin-level or platform: that would
// leave a shared, uncapped template in one tenant's bucket.
func TestOwnStorageTemplateCannotBePromoted(t *testing.T) {
	for _, own := range []bool{true, false} {
		h, st := newTestHandler(t, nil)
		tpl := seedTemplate(t, st, ownerID, "personal")
		if own {
			ownStorage(st)
			id := 5
			st.templates[tpl.ID].BackupStorageID = &id
		}
		rec := httptest.NewRecorder()
		h.Update(rec, withID(asUser(httptest.NewRequest("PATCH", "/x", strings.NewReader(`{"visibility":"platform"}`)), adminID, true), tpl.ID))
		want, vis := http.StatusOK, "platform"
		if own {
			want, vis = http.StatusConflict, "personal"
		}
		if rec.Code != want || st.templates[tpl.ID].Visibility != vis {
			t.Errorf("own=%v: got %d (%s) visibility %q, want %d %q", own, rec.Code, rec.Body, st.templates[tpl.ID].Visibility, want, vis)
		}
	}
}
