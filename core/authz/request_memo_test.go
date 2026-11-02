package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
)

// RequireCap hands the server row it decided on to the handler: the handler's
// first read of that server costs nothing, gets a copy, and any other server
// is still read from the store. A denied request never reaches the handler.
func TestRequireCapHandsTheServerRowToTheHandler(t *testing.T) {
	for _, tc := range []struct {
		name      string
		id        Identity
		wantRun   bool
		wantReads int // GetServerByID through middleware + handler
	}{
		{"owner", Identity{UserID: "owner"}, true, 2}, // decide + read server 2
		{"admin", Identity{UserID: "admin", IsAdmin: true}, true, 2},
		{"stranger is denied", Identity{UserID: "stranger"}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := newCounting(&resolverFakeStore{servers: map[int]*models.Server{
				1: {ID: 1, Name: "one", OwnerID: "owner", NodeOwnerLoaded: true},
				2: {ID: 2, Name: "two", OwnerID: "owner"},
			}})
			r := NewResolver(cs)
			r.SetForeignNode(func(*models.Server, string) bool { return false })
			ran := false
			h := r.RequireCap("console.read")(func(w http.ResponseWriter, req *http.Request) {
				ran = true
				srv, err := ServerByID(req.Context(), cs, 1)
				if err != nil || srv.Name != "one" {
					t.Fatalf("memo read: %+v %v", srv, err)
				}
				srv.Name = "edited"
				again, _ := ServerByID(req.Context(), cs, 1)
				if again.Name != "one" {
					t.Fatal("a handler's edit reached the memo")
				}
				if other, _ := ServerByID(req.Context(), cs, 2); other == nil || other.Name != "two" {
					t.Fatal("another server must come from the store")
				}
			})
			rec := httptest.NewRecorder()
			h(rec, reqWithIdentity(tc.id, map[string]string{"id": "1"}))
			if ran != tc.wantRun {
				t.Fatalf("handler ran=%v (status %d), want %v", ran, rec.Code, tc.wantRun)
			}
			if got := cs.calls["GetServerByID"]; got != tc.wantReads {
				t.Fatalf("GetServerByID calls %d, want %d", got, tc.wantReads)
			}
		})
	}
}

// Without RequireCap in front (no memo), ServerByID is the store read.
func TestServerByIDWithoutMemoReadsTheStore(t *testing.T) {
	cs := newCounting(&resolverFakeStore{servers: map[int]*models.Server{1: {ID: 1}}})
	if _, err := ServerByID(context.Background(), cs, 1); err != nil || cs.calls["GetServerByID"] != 1 {
		t.Fatalf("err %v calls %v", err, cs.calls)
	}
}
