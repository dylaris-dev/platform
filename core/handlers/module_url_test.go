package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

type moduleCreateStore struct {
	store.Store
	created int
}

func (f *moduleCreateStore) CreateModule(*models.Module) (int, error) {
	f.created++
	return 9, nil
}

// An iframe module is shown to every user it is visible to, and its address
// was taken as sent. It now follows the custom-tab rule: http or https.
func TestAnIframeModuleNeedsAnHTTPAddress(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"name":"x","type":"iframe","url":"javascript:alert(1)"}`, http.StatusBadRequest},
		{`{"name":"x","type":"iframe","url":"data:text/html,hi"}`, http.StatusBadRequest},
		{`{"name":"x","type":"iframe","url":""}`, http.StatusBadRequest},
		{`{"name":"x","type":"iframe","url":"https://status.example.com/"}`, http.StatusOK},
	} {
		fs := &moduleCreateStore{}
		h := &ModuleHandler{state: &AppState{Store: fs}}
		rec := httptest.NewRecorder()
		h.CreateModuleHandler(rec, httptest.NewRequest("POST", "/api/modules", strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.body, rec.Code, tc.want)
		}
		if tc.want != http.StatusOK && fs.created != 0 {
			t.Errorf("%s: the module was created anyway", tc.body)
		}
	}
}
