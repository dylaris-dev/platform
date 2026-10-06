package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"dylaris-core/pkg/crypto"
)

// With no relay registered - which is also every relay restart - the ticket
// carried the node's public address to anyone who could open the server, while
// the server list hides it from them. The ticket now follows the list's rule.
func TestBeamTicketFollowsTheNodeAddressRule(t *testing.T) {
	enc, err := crypto.Encrypt(crypto.DeriveKey("test-cluster-secret", "node-redis-secret"), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, routing, files string
		admin, ownsNode      bool
		wantPublic           bool
	}{
		{"tenant, gateway, beam-only", "gateway", "beam", false, false, false},
		{"tenant, gateway, sftp", "gateway", "sftp", false, false, true},
		{"tenant, direct routing", "ip_port", "beam", false, false, true},
		{"admin", "gateway", "beam", true, false, true},
		{"owner of the node", "gateway", "beam", false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newBeamAccessStore(nil, "owner-id")
			fs.node.PublicIP = "203.0.113.9"
			fs.node.PrivateIPs = []string{"10.0.0.5"}
			if c.ownsNode {
				owner := "owner-id"
				fs.node.OwnerID = &owner
			}
			fs.secretEnc = enc
			fs.settings = map[string]string{"routing_mode": c.routing, "file_access_mode": c.files}
			h := newBeamAccessHandler(fs)

			r := httptest.NewRequest("GET", "/api/beam/ticket?server_uuid=srv-uuid", nil)
			ctx := context.WithValue(r.Context(), "username", "owner")
			ctx = context.WithValue(ctx, "isAdmin", c.admin)
			ctx = context.WithValue(ctx, "userID", "owner-id")
			rec := httptest.NewRecorder()
			h.GetBeamTicket(rec, r.WithContext(ctx))

			var body struct {
				Hints *beamDirectHints `json:"lanHints"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != 200 || body.Hints == nil || len(body.Hints.IPs) == 0 {
				t.Fatalf("status=%d, want a ticket with LAN hints (body=%s)", rec.Code, rec.Body.String())
			}
			if got := body.Hints.PublicAddr != ""; got != c.wantPublic {
				t.Errorf("public address present = %v, want %v (%q)", got, c.wantPublic, body.Hints.PublicAddr)
			}
		})
	}
}
