package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/gorilla/mux"

	"dylaris-core/models"
)

// Test helpers shared across handlers tests. They used to live in node test
// files and moved back here when the node handlers became handlers/nodes.

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
