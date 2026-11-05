package beam

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"

	beamauth "dylaris-pkg/beam/auth"
)

// flippingStore answers the server as it was when the request started on the
// first read, and as 'migrating' on every later one: the move's status write
// lands while the ticket handler is between its first read and the signing.
type flippingStore struct {
	*beamAccessFakeStore
	reads int
}

func (f *flippingStore) GetServerByUUID(uuid string) (*models.Server, error) {
	f.reads++
	srv, err := f.beamAccessFakeStore.GetServerByUUID(uuid)
	if err != nil || f.reads == 1 {
		return srv, err
	}
	moved := *srv
	moved.Status = "migrating"
	return &moved, nil
}

// A ticket must not carry write or delete when the server started moving before
// it was signed. The move stamps the access epoch once, right after it writes
// 'migrating'; a ticket signed in a later second than that stamp passes it, so
// a write right taken from a status read made before the move kept read-write
// access to a source copy that is deleted at cutover.
func TestBeamTicketSignedAfterAMoveStartedIsReadOnly(t *testing.T) {
	cases := []struct {
		name    string
		initial string
		wantRW  bool
	}{
		{"server online throughout", "", true},
		{"server starts moving before the ticket is signed", "online", false},
		{"server already moving", "migrating", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := newBeamAccessStore(nil, "owner-id")
			base.server.Status = c.initial
			h := newBeamAccessHandler(base)
			if c.initial == "online" {
				h.d.Store = &flippingStore{beamAccessFakeStore: base}
			}

			r := httptest.NewRequest("GET", "/api/beam/ticket?server_uuid=srv-uuid", nil)
			ctx := context.WithValue(r.Context(), "username", "owner")
			ctx = context.WithValue(ctx, "isAdmin", false)
			ctx = context.WithValue(ctx, "userID", "owner-id")
			rec := httptest.NewRecorder()
			h.GetBeamTicket(rec, r.WithContext(ctx))

			var body struct {
				Ticket string `json:"ticket"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != 200 || body.Ticket == "" {
				t.Fatalf("status=%d, want a ticket (body=%s)", rec.Code, rec.Body.String())
			}
			claims, err := beamauth.ValidateBeamTicket("beam-test-secret", body.Ticket)
			if err != nil {
				t.Fatalf("ticket does not validate: %v", err)
			}
			if claims.Perms == nil {
				t.Fatal("ticket carries no permissions")
			}
			if rw := claims.Perms.Write || claims.Perms.Delete; rw != c.wantRW {
				t.Fatalf("write/delete in ticket = %v, want %v (perms %+v)", rw, c.wantRW, *claims.Perms)
			}
			if !claims.Perms.Read {
				t.Fatal("read was taken away too")
			}
		})
	}
}

// revokingStore applies an access change the first time the handler looks up
// the node, which is after the access checks and before the signing: the
// owner's change commits and stamps the access epoch in that window.
type revokingStore struct {
	*beamAccessFakeStore
	revoke func(*beamAccessFakeStore)
	done   bool
}

func (f *revokingStore) GetNodeByID(id int) (*models.Node, error) {
	if !f.done {
		f.done = true
		f.revoke(f.beamAccessFakeStore)
	}
	return f.beamAccessFakeStore.GetNodeByID(id)
}

// Every access change stamps the epoch once, after its commit. A ticket signed
// in a later second than that stamp passes it, so rights decided before the
// change must not survive into a ticket signed after it - for every source of
// the stamp, not only a move.
func TestBeamTicketSignedAfterAnAccessChangeFollowsTheChange(t *testing.T) {
	builder := []string{"files.read", "files.write", "files.delete", "sftp.access"}
	cases := []struct {
		name     string
		revoke   func(*beamAccessFakeStore)
		wantCode int
		wantRW   bool
	}{
		{"nothing changes", func(*beamAccessFakeStore) {}, 200, true},
		{"member removed", func(f *beamAccessFakeStore) { f.grant = nil }, 403, false},
		{"downgraded to read-only", func(f *beamAccessFakeStore) {
			f.role.Capabilities = []string{"files.read", "sftp.access"}
		}, 200, false},
		{"owner suspended", func(f *beamAccessFakeStore) { f.billingStatus = "suspended" }, 403, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := newBeamAccessStore(builder, "member-id")
			h := newBeamAccessHandler(base)
			h.d.Store = &revokingStore{beamAccessFakeStore: base, revoke: c.revoke}

			r := httptest.NewRequest("GET", "/api/beam/ticket?server_uuid=srv-uuid", nil)
			ctx := context.WithValue(r.Context(), "username", "member")
			ctx = context.WithValue(ctx, "isAdmin", false)
			ctx = context.WithValue(ctx, "userID", "member-id")
			rec := httptest.NewRecorder()
			h.GetBeamTicket(rec, r.WithContext(ctx))

			var body struct {
				Ticket string `json:"ticket"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != c.wantCode {
				t.Fatalf("status=%d, want %d (body=%s)", rec.Code, c.wantCode, rec.Body.String())
			}
			if c.wantCode != 200 {
				if body.Ticket != "" {
					t.Fatal("a revoked caller was still handed a ticket")
				}
				return
			}
			claims, err := beamauth.ValidateBeamTicket("beam-test-secret", body.Ticket)
			if err != nil {
				t.Fatalf("ticket does not validate: %v", err)
			}
			if claims.Perms == nil || !claims.Perms.Read {
				t.Fatalf("ticket lost read: %+v", claims.Perms)
			}
			if rw := claims.Perms.Write || claims.Perms.Delete; rw != c.wantRW {
				t.Fatalf("write/delete in ticket = %v, want %v (perms %+v)", rw, c.wantRW, *claims.Perms)
			}
		})
	}
}
