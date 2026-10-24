package tickets

import (
	"strings"
	"testing"
)

// TestCannedRequestValidate pins the name/body boundary checks (1..128,
// 1..10000) on the canned-response create/update payload.
func TestCannedRequestValidate(t *testing.T) {
	catID := 7

	t.Run("empty name rejected", func(t *testing.T) {
		req := cannedRequest{Name: "", Body: "hello"}
		if _, err := req.validate(); err == nil {
			t.Fatalf("expected error for empty name")
		}
	})
	t.Run("name over 128 chars rejected", func(t *testing.T) {
		req := cannedRequest{Name: strings.Repeat("a", 129), Body: "hello"}
		if _, err := req.validate(); err == nil {
			t.Fatalf("expected error for over-length name")
		}
	})
	t.Run("name exactly 128 chars accepted", func(t *testing.T) {
		req := cannedRequest{Name: strings.Repeat("a", 128), Body: "hello"}
		if _, err := req.validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("empty body rejected", func(t *testing.T) {
		req := cannedRequest{Name: "greeting", Body: ""}
		if _, err := req.validate(); err == nil {
			t.Fatalf("expected error for empty body")
		}
	})
	t.Run("body over 10000 chars rejected", func(t *testing.T) {
		req := cannedRequest{Name: "greeting", Body: strings.Repeat("b", 10001)}
		if _, err := req.validate(); err == nil {
			t.Fatalf("expected error for over-length body")
		}
	})
	t.Run("body exactly 10000 chars accepted", func(t *testing.T) {
		req := cannedRequest{Name: "greeting", Body: strings.Repeat("b", 10000)}
		if _, err := req.validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("valid request trims whitespace and carries fields through", func(t *testing.T) {
		req := cannedRequest{Name: "  Greeting  ", Body: "  Hello there  ", CategoryID: &catID}
		got, err := req.validate()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Name != "Greeting" || got.Body != "Hello there" {
			t.Fatalf("got Name=%q Body=%q, want trimmed values", got.Name, got.Body)
		}
		if got.CategoryID == nil || *got.CategoryID != catID {
			t.Fatalf("got CategoryID=%v, want %d", got.CategoryID, catID)
		}
	})
}
