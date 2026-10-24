package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
)

// Test helpers shared across handlers tests. They used to live in node test
// files and moved back here when the node handlers became handlers/nodes.

// funcBody returns the text of one top-level func, from its signature to the
// closing brace in column 0.
func funcBody(t *testing.T, file, fn string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	re := regexp.MustCompile(`(?ms)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(fn) + `\(.*?^}`)
	body := re.FindString(string(src))
	if body == "" {
		t.Fatalf("%s not found in %s", fn, file)
	}
	return body
}

const (
	visOwner = "11111111-1111-1111-1111-111111111111"
	visAdmin = "22222222-2222-2222-2222-222222222222"
)

func visOwnedNode() *models.Node {
	owner := visOwner
	return &models.Node{ID: 7, Status: "online", OwnerID: &owner}
}

func visPlatformNode() *models.Node {
	return &models.Node{ID: 7, Status: "online"}
}

func visAdminRequest(method, target string, vars map[string]string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	ctx := context.WithValue(r.Context(), "isAdmin", true)
	ctx = context.WithValue(ctx, "userID", visAdmin)
	return mux.SetURLVars(r.WithContext(ctx), vars)
}
