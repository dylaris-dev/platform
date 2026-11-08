package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
)

func (f *extStore) SetSetting(k, v string) error {
	f.settings[k] = v
	return nil
}

func (f *extStore) DeleteSetting(k string) error {
	delete(f.settings, k)
	return nil
}

var extAudit []string

func (f *extStore) InsertAuditIdentity(ev *models.AuditEventIdentity) error {
	extAudit = append(extAudit, ev.EventType)
	return nil
}

func (f *extStore) GetUserByID(id string) (*models.User, error) {
	if id == "ghost" {
		return nil, errors.New("not found")
	}
	return &models.User{ID: id}, nil
}

func TestValidateExternalTarget(t *testing.T) {
	const uuid = "0a1b2c3d-0000-4000-8000-0123456789ab"
	cases := []struct {
		host   string
		public bool
		want   error // nil, errTargetNotAllowed, or any other non-nil
	}{
		{"10.1.2.3", false, nil},
		{"172.16.0.1", false, nil},
		{"172.31.255.255", false, nil},
		{"192.168.1.5", false, nil},
		{"127.0.0.1", false, nil},
		{"100.64.0.9", false, nil},
		{"100.127.255.255", false, nil},
		{"fd12:3456::1", false, nil},
		{"localhost", false, nil},
		{"mc-" + uuid, false, nil},
		{"MC-" + uuid, false, nil},
		{"172.32.0.1", false, errTargetNotAllowed},
		{"100.128.0.1", false, errTargetNotAllowed},
		{"8.8.8.8", false, errTargetNotAllowed},
		{"2001:db8::1", false, errTargetNotAllowed},
		{"::ffff:8.8.8.8", false, errTargetNotAllowed},
		{"play.example.com", false, errTargetNotAllowed},
		{"mc_" + uuid, false, errors.New("any")}, // refused earlier as a malformed host
		{"mc-" + uuid + "x", false, errTargetNotAllowed},
		{"::1", false, errors.New("any")},
		{"", false, errors.New("any")},
		{"8.8.8.8", true, nil},
		{"2001:db8::1", true, nil},
		{"play.example.com", true, nil},
		{"::1", true, errors.New("any")},
	}
	for _, c := range cases {
		err := validateExternalTarget(c.host, c.public)
		switch {
		case c.want == nil && err != nil, c.want != nil && err == nil:
			t.Errorf("validateExternalTarget(%q, %v) = %v, want %v", c.host, c.public, err, c.want)
		case c.want == errTargetNotAllowed && !errors.Is(err, errTargetNotAllowed):
			t.Errorf("validateExternalTarget(%q, %v) = %v, want errTargetNotAllowed", c.host, c.public, err)
		}
	}
}

func TestExternalCreateRouteRespectsPublicFlag(t *testing.T) {
	body := map[string]interface{}{"linkId": "link-b", "domain": "x.example.com", "targetHost": "8.8.8.8"}

	f := newExtFixture(t)
	rec := httptest.NewRecorder()
	f.h.CreateRoute(rec, extReq("POST", "/", extTenantB, false, nil, body))
	out := decodeExt(t, rec)
	if rec.Code != http.StatusBadRequest || out["code"] != "target_not_allowed" || out["success"] != false {
		t.Fatalf("public target without the flag = %d %s", rec.Code, rec.Body.String())
	}
	if out["error"] != targetNotAllowedMsg {
		t.Fatalf("error = %q", out["error"])
	}

	// The flag is per account: tenant A's flag does not open tenant B.
	f.st.settings[publicTargetsKey(extTenantA)] = "true"
	rec = httptest.NewRecorder()
	f.h.CreateRoute(rec, extReq("POST", "/", extTenantB, false, nil, body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("another account's flag opened this one: %d %s", rec.Code, rec.Body.String())
	}

	f.st.settings[publicTargetsKey(extTenantB)] = "true"
	rec = httptest.NewRecorder()
	f.h.CreateRoute(rec, extReq("POST", "/", extTenantB, false, nil, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("public target with the flag = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	// A private target needs no flag, and the body still reaches the handler intact.
	f = newExtFixture(t)
	body["targetHost"] = "192.168.1.5"
	rec = httptest.NewRecorder()
	f.h.CreateRoute(rec, extReq("POST", "/", extTenantB, false, nil, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("private target = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

func TestPanelCreateRouteUnchanged(t *testing.T) {
	f := newExtFixture(t)
	body := map[string]interface{}{"linkId": "link-b", "domain": "x.example.com", "targetHost": "8.8.8.8"}
	rec := httptest.NewRecorder()
	f.h.gw.CreateLinkRoute(rec, extReq("POST", "/", extTenantB, false, nil, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("session path with a public target = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminTogglePublicTargets(t *testing.T) {
	const target = "0a1b2c3d-0000-4000-8000-0123456789ab"
	vars := map[string]string{"id": target}
	extAudit = nil
	f := newExtFixture(t)
	gh := f.h.gw

	get := func() bool {
		rec := httptest.NewRecorder()
		gh.GetPublicTargets(rec, extReq("GET", "/", "admin", true, vars, nil))
		var out map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out["enabled"] == true
	}
	put := func(body interface{}, v map[string]string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		gh.SetPublicTargets(rec, extReq("PUT", "/", "admin", true, v, body))
		return rec
	}

	if get() {
		t.Fatal("default must be off")
	}
	if rec := put(map[string]bool{"enabled": true}, vars); rec.Code != 200 || !get() {
		t.Fatalf("enable = %d %s", rec.Code, rec.Body.String())
	}
	if f.st.settings[publicTargetsKey(target)] != "true" {
		t.Fatal("setting not written under the documented key")
	}
	if rec := put(map[string]bool{"enabled": false}, vars); rec.Code != 200 || get() {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := f.st.settings[publicTargetsKey(target)]; ok {
		t.Fatal("disable must delete the setting")
	}
	if len(extAudit) != 2 || extAudit[0] != "gateway_public_targets_changed" {
		t.Fatalf("audit rows = %v, want two gateway_public_targets_changed", extAudit)
	}
	if rec := put(map[string]string{}, vars); rec.Code != 400 {
		t.Fatalf("missing enabled = %d, want 400", rec.Code)
	}
	if rec := put(map[string]bool{"enabled": true}, map[string]string{"id": "nope"}); rec.Code != 400 {
		t.Fatalf("bad id = %d, want 400", rec.Code)
	}
}

// The session handler reads only the first JSON value, so the policy must judge
// that same value and refuse anything it cannot decode.
func TestExternalCreateRouteFailsClosedOnBodyShape(t *testing.T) {
	const pub = `{"linkId":"link-b","domain":"x.example.com","targetHost":"8.8.8.8"}`
	cases := []struct {
		name, body, code string
	}{
		{"trailing garbage after a public target", pub + " x", "target_not_allowed"},
		{"second object after a public target", pub + "{}", "target_not_allowed"},
		{"malformed json", `{"targetHost":"8.8.8.8"`, "invalid_request"},
		{"type mismatch", `{"linkId":"link-b","domain":"x.example.com","targetHost":5}`, "invalid_request"},
		{"empty body", ``, "invalid_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newExtFixture(t)
			r := extReq("POST", "/", extTenantB, false, nil, nil)
			r.Body = io.NopCloser(strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			f.h.CreateRoute(rec, r)
			out := decodeExt(t, rec)
			if rec.Code != http.StatusBadRequest || out["code"] != c.code || out["success"] != false {
				t.Fatalf("got %d %s, want 400 %s", rec.Code, rec.Body.String(), c.code)
			}
			if len(f.gw.createRouteViaLinkCalls) != 0 {
				t.Fatalf("a route was created: %v", f.gw.createRouteViaLinkCalls)
			}
		})
	}
}
