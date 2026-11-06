package beam

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/handlers/reqctx"

	beamauth "dylaris-pkg/beam/auth"
)

// The ticket's iat is the moment the caller's identity was read, not when it
// was signed. The node refuses a ticket whose iat is no later than the access
// stamp, so every change that commits and stamps after that read - a demotion,
// a password or 2FA reset, a deletion, which the re-check cannot see because it
// decides from the same identity - refuses the ticket. A re-sign after the
// re-check narrowed it keeps that iat too, instead of taking a fresh one that a
// change between the re-check and the re-sign would pass.
func TestBeamTicketIsDatedWhenTheIdentityWasRead(t *testing.T) {
	builder := []string{"files.read", "files.write", "files.delete", "sftp.access"}
	cases := []struct {
		name   string
		revoke func(*beamAccessFakeStore)
	}{
		{"nothing changes", func(*beamAccessFakeStore) {}},
		{"re-check narrows and re-signs", func(f *beamAccessFakeStore) {
			f.role.Capabilities = []string{"files.read", "sftp.access"}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := newBeamAccessStore(builder, "member-id")
			h := newBeamAccessHandler(base)
			h.d.Store = &revokingStore{beamAccessFakeStore: base, revoke: c.revoke}

			loaded := time.Now().Add(-5 * time.Second)
			r := httptest.NewRequest("GET", "/api/beam/ticket?server_uuid=srv-uuid", nil)
			ctx := context.WithValue(r.Context(), "username", "member")
			ctx = context.WithValue(ctx, "isAdmin", false)
			ctx = context.WithValue(ctx, "userID", "member-id")
			ctx = reqctx.WithIdentityLoadedAt(ctx, loaded)
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
			if claims.IssuedAt == nil || claims.IssuedAt.Unix() != loaded.Unix() {
				t.Fatalf("iat = %v, want the identity read at %v", claims.IssuedAt, loaded)
			}
		})
	}
}
