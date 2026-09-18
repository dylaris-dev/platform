package services

import (
	"strings"
	"testing"
)

// The rule this decides: what the DNS report says about the API name.
//
// It matters because the answer CHANGED. Core serves the panel itself now, so
// the browser calls /api on the origin it was loaded from and there is no
// second name to create. The row was still built from core_public_url - the
// base a node downloads a built pack from, with nothing to do with what the
// browser calls - and told an operator with a perfectly working panel that no
// API address was set and that the panel would "load and then do nothing".
//
// Both halves have to keep working: the split-hostname deployment is still
// supported, and for it the name IS load-bearing.
func TestAPIOriginRow(t *testing.T) {
	t.Run("the same-origin default has nothing to configure", func(t *testing.T) {
		row, separate := apiOriginRow("", "panel.example.com")
		if separate {
			t.Fatal("a second API name was reported where none exists")
		}
		if strings.Contains(row.Hint, "Modpacks") {
			t.Errorf("the hint sends the operator to a setting that does not decide this: %q", row.Hint)
		}
		if strings.Contains(row.Name, "not configured") {
			t.Errorf("Name = %q reads as a gap; the same origin IS the configuration", row.Name)
		}
		if row.Status != "info" {
			t.Errorf("Status = %q, want info: nothing here is wrong", row.Status)
		}
		if !strings.Contains(row.Hint, "PANEL_API_URL") {
			t.Errorf("the hint does not name the one thing that changes this: %q", row.Hint)
		}
	})

	// PANEL_API_URL pointing at the panel's own host is the same deployment
	// spelled out by hand. It must not produce a duplicate row that resolves the
	// name the panel row already resolved.
	t.Run("an API url equal to the panel host is still one origin", func(t *testing.T) {
		if _, separate := apiOriginRow("panel.example.com", "panel.example.com"); separate {
			t.Error("the panel host was reported as a separate API name")
		}
	})

	t.Run("a second hostname is checked", func(t *testing.T) {
		row, separate := apiOriginRow("api.example.com", "panel.example.com")
		if !separate {
			t.Fatal("a genuinely separate API host was not checked")
		}
		if row.Name != "api.example.com" {
			t.Errorf("Name = %q, want the API host", row.Name)
		}
	})
}
