package beam

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/store"
)

func (f *beamAccessFakeStore) GetUserDataBudget(string) (string, error) { return f.dataBudget, nil }

func (f *beamAccessFakeStore) GetTrafficUsageRegions(string, time.Time) ([]store.RegionUsage, error) {
	return []store.RegionUsage{{Region: "eu", Kind: store.TrafficKindRelay, Product: store.TrafficProductBYON, Bytes: f.relayBytes}}, nil
}

// The owner's self-set data budget gates the ticket: past it only the owner or
// a panel admin gets one, and only with the explicit ack the app sends after
// its confirm dialog.
func TestGetBeamTicket_DataBudget(t *testing.T) {
	builder := []string{"files.read", "files.write", "sftp.access"}
	cases := []struct {
		name     string
		budget   string
		used     int64
		userID   string
		isAdmin  bool
		caps     []string
		query    string
		wantCode string // "" = a ticket
	}{
		{"budget unset: no gate", "", 9_000_000_000_000, "member-id", false, builder, "", ""},
		{"budget unlimited: no gate", "unlimited", 9_000_000_000_000, "member-id", false, builder, "", ""},
		{"below the budget: no gate", "1000", 999_000_000_000, "member-id", false, builder, "", ""},
		{"reached, member: refused", "1000", 1_000_000_000_000, "member-id", false, builder, "", "data_budget_reached"},
		{"reached, member with ack: still refused", "1000", 1_000_000_000_000, "member-id", false, builder, "&ack_over_budget=true", "data_budget_reached"},
		{"reached, owner without ack", "1000", 1_000_000_000_000, "owner-id", false, nil, "", "data_budget_ack_required"},
		{"reached, owner with ack", "1000", 1_000_000_000_000, "owner-id", false, nil, "&ack_over_budget=true", ""},
		{"reached, admin without ack", "1000", 1_000_000_000_000, "admin-id", true, nil, "", "data_budget_ack_required"},
		{"reached, admin with ack", "1000", 1_000_000_000_000, "admin-id", true, nil, "&ack_over_budget=true", ""},
		{"zero budget: owner needs the ack at 0 bytes", "0", 0, "owner-id", false, nil, "", "data_budget_ack_required"},
		{"zero budget: ack passes", "0", 0, "owner-id", false, nil, "&ack_over_budget=true", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newBeamAccessStore(c.caps, c.userID)
			fs.dataBudget, fs.relayBytes = c.budget, c.used
			h := newBeamAccessHandler(fs)

			r := httptest.NewRequest("GET", "/api/beam/ticket?server_uuid=srv-uuid"+c.query, nil)
			ctx := context.WithValue(r.Context(), "username", "u")
			ctx = context.WithValue(ctx, "isAdmin", c.isAdmin)
			ctx = context.WithValue(ctx, "userID", c.userID)
			rec := httptest.NewRecorder()
			h.GetBeamTicket(rec, r.WithContext(ctx))

			var body struct {
				Ticket string `json:"ticket"`
				Code   string `json:"code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if c.wantCode == "" {
				if rec.Code != 200 || body.Ticket == "" {
					t.Fatalf("status=%d, want a ticket (body=%s)", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != 403 || body.Code != c.wantCode || body.Ticket != "" {
				t.Fatalf("status=%d code=%q, want 403 %q and no ticket (body=%s)", rec.Code, body.Code, c.wantCode, rec.Body.String())
			}
		})
	}
}

// GetBeamConfig reports the budget of the server's owner, and whether the
// caller may confirm past it; a caller who may not beam to the server gets none.
func TestGetBeamConfig_DataBudget(t *testing.T) {
	cases := []struct {
		name        string
		userID      string
		caps        []string
		query       string
		wantNull    bool
		wantConfirm bool
	}{
		{"owner of the server", "owner-id", nil, "?server_uuid=srv-uuid", false, true},
		{"member of the server", "member-id", []string{"files.read", "files.write", "sftp.access"}, "?server_uuid=srv-uuid", false, false},
		{"no access to the server", "member-id", nil, "?server_uuid=srv-uuid", true, false},
		{"own budget without a server", "member-id", nil, "", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newBeamAccessStore(c.caps, c.userID)
			fs.dataBudget, fs.relayBytes = "1000", 1_500_000_000_000
			h := newBeamAccessHandler(fs)
			r := httptest.NewRequest("GET", "/api/beam/config"+c.query, nil)
			ctx := context.WithValue(r.Context(), "username", "u")
			ctx = context.WithValue(ctx, "isAdmin", false)
			ctx = context.WithValue(ctx, "userID", c.userID)
			rec := httptest.NewRecorder()
			h.GetBeamConfig(rec, r.WithContext(ctx))

			var body struct {
				DataBudget *struct {
					BudgetGB   *int64 `json:"budget_gb"`
					UsedGB     int64  `json:"used_gb"`
					Reached    bool   `json:"reached"`
					CanConfirm bool   `json:"can_confirm"`
				} `json:"data_budget"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if c.wantNull {
				if body.DataBudget != nil {
					t.Fatalf("data_budget = %+v, want null", body.DataBudget)
				}
				return
			}
			d := body.DataBudget
			if d == nil || d.BudgetGB == nil || *d.BudgetGB != 1000 || d.UsedGB != 1500 || !d.Reached || d.CanConfirm != c.wantConfirm {
				t.Fatalf("data_budget = %+v (body=%s)", d, rec.Body.String())
			}
		})
	}
}
