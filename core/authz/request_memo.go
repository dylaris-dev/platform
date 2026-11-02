package authz

import (
	"context"

	"dylaris-core/models"
)

// Request-scoped memos. Each lives on ONE request's context and dies with it:
// nothing here is shared between requests, so a revoked grant, a suspended
// owner or a changed row is seen by the very next request exactly as before.
// What it saves is the same row being read twice inside one request.

type userMemoKey struct{}

// WithUser records the caller's user row as the auth middleware loaded it.
func WithUser(ctx context.Context, u *models.User) context.Context {
	if u == nil {
		return ctx
	}
	cp := *u
	return context.WithValue(ctx, userMemoKey{}, &cp)
}

// UserFromContext returns a COPY of the row WithUser recorded, or nil. A copy,
// because handlers clear fields (the password hash) before writing it out.
func UserFromContext(ctx context.Context) *models.User {
	u, _ := ctx.Value(userMemoKey{}).(*models.User)
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}

type serverMemoKey struct{}

// serverReader is the one store method ServerByID falls back to.
type serverReader interface {
	GetServerByID(id int) (*models.Server, error)
}

// ServerByID returns the server row RequireCap loaded to authorize THIS
// request, when it is the server asked for, and reads the store otherwise.
//
// Only for a handler's first read of its own server: a handler that writes
// the row and then reads it back must read the store, or it gets the row as it
// was before its own write.
func ServerByID(ctx context.Context, st serverReader, id int) (*models.Server, error) {
	if srv, _ := ctx.Value(serverMemoKey{}).(*models.Server); srv != nil && srv.ID == id {
		cp := *srv
		return &cp, nil
	}
	return st.GetServerByID(id)
}

func withServer(ctx context.Context, srv *models.Server) context.Context {
	if srv == nil {
		return ctx
	}
	cp := *srv
	return context.WithValue(ctx, serverMemoKey{}, &cp)
}
