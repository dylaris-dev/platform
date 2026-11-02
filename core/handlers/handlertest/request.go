package handlertest

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/gorilla/mux"
)

// The callers the node visibility, foreign-machine and staff-bound tests act
// as. VisOwner owns the customer machine OwnedNode returns.
const (
	VisOwner = "11111111-1111-1111-1111-111111111111"
	VisAdmin = "22222222-2222-2222-2222-222222222222"
	StaffID  = "33333333-3333-3333-3333-333333333333"
)

// AdminRequest is a request from the admin VisAdmin, username "admin", to
// target "/x" with the given mux vars and body.
func AdminRequest(method string, vars map[string]string, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	ctx := context.WithValue(r.Context(), "isAdmin", true)
	ctx = context.WithValue(ctx, "userID", VisAdmin)
	ctx = context.WithValue(ctx, "username", "admin")
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

// StaffRequest is AdminRequest for StaffID, username "staff", who is not an
// admin.
func StaffRequest(method string, vars map[string]string, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	ctx := context.WithValue(r.Context(), "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", StaffID)
	ctx = context.WithValue(ctx, "username", "staff")
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

// VisAdminRequest is a body-less request from VisAdmin that carries no
// username, as the node visibility tests have always sent it.
func VisAdminRequest(method, target string, vars map[string]string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	ctx := context.WithValue(r.Context(), "isAdmin", true)
	ctx = context.WithValue(ctx, "userID", VisAdmin)
	return mux.SetURLVars(r.WithContext(ctx), vars)
}
