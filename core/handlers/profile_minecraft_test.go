package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Minecraft name only picks the avatar head, so its own door asks for no
// password; it must still refuse what the profile form refuses.
func TestUpdateMinecraftUsernameHandler(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantCode  int
		wantWrite []string
	}{
		{"sets without a password", `{"minecraftUsername":" Notch "}`, http.StatusOK, []string{"Notch"}},
		{"empty clears", `{"minecraftUsername":""}`, http.StatusOK, []string{""}},
		{"too short", `{"minecraftUsername":"ab"}`, http.StatusBadRequest, nil},
		{"space inside", `{"minecraftUsername":"No tch"}`, http.StatusBadRequest, nil},
		{"too long", `{"minecraftUsername":"abcdefghijklmnopq"}`, http.StatusBadRequest, nil},
		{"field missing", `{}`, http.StatusBadRequest, nil},
		{"not json", `nope`, http.StatusBadRequest, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newProfileStore(t, false)
			h := NewAuthHandler(&AppState{Store: st}, "test-secret")
			r := httptest.NewRequest(http.MethodPut, "/api/auth/profile/minecraft", bytes.NewReader([]byte(c.body)))
			r = r.WithContext(context.WithValue(r.Context(), "username", "me"))
			w := httptest.NewRecorder()
			h.UpdateMinecraftUsernameHandler(w, r)
			if w.Code != c.wantCode {
				t.Fatalf("status %d, want %d: %s", w.Code, c.wantCode, w.Body.String())
			}
			if len(st.mcWrites) != len(c.wantWrite) || (len(c.wantWrite) == 1 && st.mcWrites[0] != c.wantWrite[0]) {
				t.Fatalf("writes = %q, want %q", st.mcWrites, c.wantWrite)
			}
			if c.wantCode == http.StatusOK {
				var out struct {
					Success           bool   `json:"success"`
					MinecraftUsername string `json:"minecraftUsername"`
				}
				json.Unmarshal(w.Body.Bytes(), &out)
				if !out.Success || out.MinecraftUsername != c.wantWrite[0] {
					t.Fatalf("body = %s", w.Body.String())
				}
			}
		})
	}
}
