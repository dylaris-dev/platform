package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// core-info is the only place the panel and the anonymous share wrapper learn
// whether proxied tabs exist at all. tabProxyAvailable is reported separately
// from the suffix so the UI never has to derive "is this configured" from a
// string it would otherwise only use to build a hostname.
func TestGetCoreInfo_IncludesTabProxyFields(t *testing.T) {
	cases := []struct {
		name          string
		suffix        string
		wantAvailable bool
	}{
		{"configured", "share.example.com", true},
		{"unconfigured", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewSystemHandler("eu", "core-1", tc.suffix)
			rec := httptest.NewRecorder()

			h.GetCoreInfo(rec, httptest.NewRequest("GET", "/api/system/core-info", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var body struct {
				Success            bool   `json:"success"`
				Region             string `json:"region"`
				CoreID             string `json:"coreId"`
				TabProxyHostSuffix string `json:"tabProxyHostSuffix"`
				TabProxyAvailable  bool   `json:"tabProxyAvailable"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !body.Success || body.Region != "eu" || body.CoreID != "core-1" {
				t.Errorf("base fields wrong: %+v", body)
			}
			if body.TabProxyHostSuffix != tc.suffix || body.TabProxyAvailable != tc.wantAvailable {
				t.Errorf("tabProxy fields = (%q,%v), want (%q,%v)",
					body.TabProxyHostSuffix, body.TabProxyAvailable, tc.suffix, tc.wantAvailable)
			}
		})
	}
}
