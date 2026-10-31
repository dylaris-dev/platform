package servers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeEulaStore struct {
	accepted bool
	readErr  error
	writes   int
}

func (f *fakeEulaStore) MinecraftEulaAccepted(string) (bool, error) { return f.accepted, f.readErr }
func (f *fakeEulaStore) AcceptMinecraftEula(string) error {
	f.writes++
	f.accepted = true
	return nil
}

func TestRequireMinecraftEula(t *testing.T) {
	cases := []struct {
		name       string
		st         *fakeEulaStore
		user       string
		accept     bool
		wantOK     bool
		wantStatus int
		wantCode   string
		wantWrites int
	}{
		{"already accepted passes without a write", &fakeEulaStore{accepted: true}, "u1", false, true, 200, "", 0},
		{"not accepted and not ticked is refused", &fakeEulaStore{}, "u1", false, false, http.StatusConflict, "minecraft_eula_required", 0},
		{"ticked now is recorded and passes", &fakeEulaStore{}, "u1", true, true, 200, "", 1},
		{"ticked again does not rewrite the date", &fakeEulaStore{accepted: true}, "u1", true, true, 200, "", 0},
		{"no user is refused", &fakeEulaStore{accepted: true}, "", true, false, http.StatusUnauthorized, "", 0},
		{"a read failure refuses rather than installs", &fakeEulaStore{readErr: errors.New("db down")}, "u1", true, false, http.StatusInternalServerError, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ok := requireMinecraftEula(rec, tc.st, tc.user, tc.accept)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok && rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantCode != "" && !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Fatalf("body %q lacks code %q", rec.Body.String(), tc.wantCode)
			}
			if tc.st.writes != tc.wantWrites {
				t.Fatalf("writes = %d, want %d", tc.st.writes, tc.wantWrites)
			}
		})
	}
}
