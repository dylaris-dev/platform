package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthzCatalog_ReturnsGroupedCatalog(t *testing.T) {
	h := NewAuthzHandler()
	rec := httptest.NewRecorder()
	h.Catalog(rec, httptest.NewRequest("GET", "/api/authz/catalog", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Success bool `json:"success"`
		Catalog []struct {
			Scope      string `json:"scope"`
			Categories []struct {
				Category     string `json:"category"`
				Capabilities []struct {
					ID    string `json:"id"`
					Label string `json:"label"`
					Verb  string `json:"verb"`
				} `json:"capabilities"`
			} `json:"categories"`
		} `json:"catalog"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success {
		t.Fatal("success = false")
	}
	if len(resp.Catalog) != 3 {
		t.Fatalf("got %d scopes, want 3", len(resp.Catalog))
	}
	if resp.Catalog[0].Scope != "server" {
		t.Fatalf("first scope = %q, want server", resp.Catalog[0].Scope)
	}
	// A known capability must be present under server -> Files.
	found := false
	for _, s := range resp.Catalog {
		for _, cat := range s.Categories {
			for _, c := range cat.Capabilities {
				if c.ID == "files.read" && c.Verb == "read" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("files.read not found in catalog payload")
	}
}
