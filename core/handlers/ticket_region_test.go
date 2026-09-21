package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
)

// ticketRegionStore adds the write side of ticket creation to the fake that
// already answers the resolver, and keeps the row that was stored.
type ticketRegionStore struct {
	*ticketAttachFakeStore
	category *models.TicketCategory
	created  *models.Ticket
}

// Creating a ticket now announces it to support. No staff, no announcement,
// which keeps these cases about the region and nothing else.
func (f *ticketRegionStore) ListTicketStaffIDs() ([]string, error) { return nil, nil }

func (f *ticketRegionStore) GetTicketCategory(int) (*models.TicketCategory, error) {
	return f.category, nil
}

func (f *ticketRegionStore) CreateTicket(t *models.Ticket) (int, error) {
	f.created = t
	return 1, nil
}

func (f *ticketRegionStore) AddTicketMessage(*models.TicketMessage) (int, error) { return 1, nil }
func (f *ticketRegionStore) InsertTicketAudit(*models.TicketAuditEvent) error    { return nil }
func (f *ticketRegionStore) GetTicket(int) (*models.Ticket, error)               { return nil, nil }

func newTicketRegionHandler(t *testing.T) (*TicketsHandler, *ticketRegionStore) {
	t.Helper()
	const ownerID = "owner-id"
	base := &ticketAttachFakeStore{
		servers: map[string]*models.Server{
			"srv-uuid": {ID: 7, UUID: "srv-uuid", OwnerID: ownerID, OwnerName: "owner", Region: "eu"},
		},
		users:  map[string]*models.User{ownerID: {ID: ownerID, Username: "owner"}},
		grants: map[string][]string{},
	}
	fs := &ticketRegionStore{
		ticketAttachFakeStore: base,
		category:              &models.TicketCategory{ID: 1, Name: "General", Enabled: true, DefaultPriority: "normal"},
	}
	return &TicketsHandler{state: &AppState{Store: fs, Authz: authz.NewResolver(fs)}}, fs
}

func postTicket(t *testing.T, h *TicketsHandler, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tickets", bytes.NewReader(raw))
	ctx := context.WithValue(req.Context(), "userID", "owner-id")
	ctx = context.WithValue(ctx, "username", "owner")
	rec := httptest.NewRecorder()
	h.CreateTicket(rec, req.WithContext(ctx))
	return rec
}

// The defect this fixes: tickets.region was the constant "default" for every
// ticket ever filed, while the support inbox renders a RegionBadge from it and
// offers a region multi-select filtering on it as soon as more than one region
// exists. On a two-region install every ticket wore a "default" badge and
// picking a real region emptied the list.
func TestATicketTakesTheRegionOfItsServer(t *testing.T) {
	h, fs := newTicketRegionHandler(t)

	rec := postTicket(t, h, map[string]interface{}{
		"categoryId":   1,
		"serverUuid":   "srv-uuid",
		"title":        "Server will not start",
		"firstMessage": "It stops right after boot.",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if fs.created == nil {
		t.Fatal("no ticket was stored")
	}
	if fs.created.Region != "eu" {
		t.Errorf("Region = %q, want %q - the region of the server the ticket names", fs.created.Region, "eu")
	}
	if fs.created.ServerRegion != "eu" {
		t.Errorf("ServerRegion = %q, want %q", fs.created.ServerRegion, "eu")
	}
}

// A ticket about no server has no region to inherit. "default" is the seeded
// region and the honest answer for a billing question.
func TestATicketWithNoServerFallsBackToDefault(t *testing.T) {
	h, fs := newTicketRegionHandler(t)

	rec := postTicket(t, h, map[string]interface{}{
		"categoryId":   1,
		"title":        "Question about my invoice",
		"firstMessage": "Which period does this cover?",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if fs.created.Region != "default" {
		t.Errorf("Region = %q, want %q", fs.created.Region, "default")
	}
	if fs.created.ServerRegion != "" {
		t.Errorf("ServerRegion = %q, want empty - there is no server to snapshot", fs.created.ServerRegion)
	}
}

// The request used to carry serverRegion and store it verbatim, so the author
// could state a region that contradicted the server they had just been
// authorised for. The field is gone; supplying it must change nothing.
func TestAClientSuppliedRegionIsIgnored(t *testing.T) {
	h, fs := newTicketRegionHandler(t)

	rec := postTicket(t, h, map[string]interface{}{
		"categoryId":   1,
		"serverUuid":   "srv-uuid",
		"serverRegion": "us",
		"title":        "Server will not start",
		"firstMessage": "It stops right after boot.",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if fs.created.ServerRegion != "eu" || fs.created.Region != "eu" {
		t.Errorf("Region=%q ServerRegion=%q - the request must not decide either",
			fs.created.Region, fs.created.ServerRegion)
	}
}
