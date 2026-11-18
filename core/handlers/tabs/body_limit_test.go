package tabs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Create, Update and the spark Record decoded their JSON bodies unbounded.
func TestTabAndSparkBodiesAreBounded(t *testing.T) {
	h, _ := newTabCapHandler(t, 5, 5, 5)
	spark := NewSparkHandler(h.d)
	// A field nobody reads pushes a well-formed body past the limit, so only
	// the bound turns it into "Invalid JSON".
	body := `{"name":"t","url":"https://example.com","pad":"` + strings.Repeat("a", 128<<10) + `"}`
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"tab create":   h.Create,
		"tab update":   h.Update,
		"spark record": spark.Record,
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			call(rec, tabPatchRequest(body))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Invalid JSON") {
				t.Fatalf("status = %d (%s), want the body refused as Invalid JSON", rec.Code, rec.Body.String())
			}
		})
	}
}
