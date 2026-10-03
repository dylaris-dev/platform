package services

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

type nodeLinkCall struct {
	nodeToken string
	enabled   bool
}

type nodeLinkFakeGateway struct {
	GatewayProvider
	calls []nodeLinkCall
	err   error
}

func (g *nodeLinkFakeGateway) LinkToken(nodeToken string) string { return "link-" + nodeToken }

func (g *nodeLinkFakeGateway) SetNodeLinkEnabled(nodeToken string, enabled bool) error {
	g.calls = append(g.calls, nodeLinkCall{nodeToken, enabled})
	return g.err
}

func nodeLinkService(b *store.UserBilling) (*BillingLifecycleService, *billingFakeStore, *nodeLinkFakeGateway) {
	fs := &billingFakeStore{
		billing: b,
		// node-b also has a self-enrolled link: both are switched, or the
		// routes move onto whichever stayed on.
		ownerNodes: []models.Node{{ID: 1, Token: "node-a"}, {ID: 2, Token: "node-b", LinkToken: "enrolled-b"}},
	}
	gw := &nodeLinkFakeGateway{}
	return &BillingLifecycleService{store: fs, suspendGrace: 48 * time.Hour, gateway: gw}, fs, gw
}

// A BYON node's link is a discovered link in the Hub, and the Hub re-wrote its
// tunnel key and its routes on every sync. The cutoff now switches it off there,
// for every node the tenant owns, and marks that it did.
func TestTheCutoffSwitchesTheTenantsNodeLinksOff(t *testing.T) {
	svc, fs, gw := nodeLinkService(&store.UserBilling{UserID: "u1", Status: "suspended"})
	now := time.Now()
	svc.dropWarpPeersOnceStopped(context.Background(), "u1", now.Add(-3*time.Hour), now)

	want := []nodeLinkCall{{"link-node-a", false}, {"link-node-b", false}, {"enrolled-b", false}}
	if !reflect.DeepEqual(gw.calls, want) {
		t.Fatalf("hub calls %+v, want %+v", gw.calls, want)
	}
	if fs.linksMarked != 1 {
		t.Fatalf("marked %d times, want 1", fs.linksMarked)
	}

	// Already off: the hourly repeat does not tell the Hub again.
	svc, _, gw = nodeLinkService(&store.UserBilling{UserID: "u1", Status: "suspended", NodeLinksOff: true})
	svc.dropWarpPeersOnceStopped(context.Background(), "u1", now.Add(-3*time.Hour), now)
	if len(gw.calls) != 0 {
		t.Fatalf("an already-off tenant was switched off again: %+v", gw.calls)
	}
}

// The cutoff asks the row it reads, not the list its pass started from: a
// payment that landed in between must not take a paying customer's players.
func TestAPaidUpTenantIsNotSwitchedOff(t *testing.T) {
	svc, fs, gw := nodeLinkService(&store.UserBilling{UserID: "u1", Status: "active"})
	now := time.Now()
	svc.dropWarpPeersOnceStopped(context.Background(), "u1", now.Add(-3*time.Hour), now)
	if len(gw.calls) != 0 || fs.linksMarked != 0 {
		t.Fatalf("a paid-up tenant was switched off: %+v", gw.calls)
	}
}

// A push the Hub never got is not recorded as done, so the next pass retries
// it instead of believing the links are off.
func TestAFailedSwitchIsNotMarked(t *testing.T) {
	svc, fs, gw := nodeLinkService(&store.UserBilling{UserID: "u1", Status: "suspended"})
	gw.err = errors.New("redis down")
	svc.setTenantNodeLinks("u1", false)
	if fs.linksMarked != 0 {
		t.Fatal("a failed switch was marked as done")
	}
}

// Paying again switches back on exactly what the cutoff switched off: nothing
// for a tenant the cutoff never touched (an operator may have switched that
// link off in the Hub), and nothing while an operator's hold stands.
func TestReactivationSwitchesBackOnOnlyWhatTheCutoffSwitchedOff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		billing store.UserBilling
		want    []nodeLinkCall
	}{
		{"switched off by the cutoff", store.UserBilling{UserID: "u1", Status: "active", NodeLinksOff: true},
			[]nodeLinkCall{{"link-node-a", true}, {"link-node-b", true}, {"enrolled-b", true}}},
		{"never switched off", store.UserBilling{UserID: "u1", Status: "active"}, nil},
		{"held by an operator", store.UserBilling{UserID: "u1", Status: "suspended", AdminHold: true, NodeLinksOff: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.billing
			svc, fs, gw := nodeLinkService(&b)
			if err := svc.Reactivate("u1"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gw.calls, tc.want) {
				t.Fatalf("hub calls %+v, want %+v", gw.calls, tc.want)
			}
			if wantCleared := len(tc.want) > 0; (fs.linksCleared == 1) != wantCleared {
				t.Fatalf("cleared %d times", fs.linksCleared)
			}
		})
	}
}

// The hourly pass switches back on what a failed Reactivate left off, and only
// for a tenant who is no longer cut off.
func TestThePassRestoresNodeLinksAReactivationMissed(t *testing.T) {
	svc, fs, gw := nodeLinkService(&store.UserBilling{UserID: "u1", Status: "active", NodeLinksOff: true})
	fs.allBilling = []store.UserBilling{
		{UserID: "u1", Status: "active", NodeLinksOff: true},
		{UserID: "u2", Status: "suspended", NodeLinksOff: true},
	}
	svc.restoreNodeLinks(context.Background())
	want := []nodeLinkCall{{"link-node-a", true}, {"link-node-b", true}, {"enrolled-b", true}}
	if !reflect.DeepEqual(gw.calls, want) {
		t.Fatalf("hub calls %+v, want %+v (the suspended tenant must stay off)", gw.calls, want)
	}
}

// An operator's suspension takes the links off at once, not at the graced
// cutoff two days later.
func TestSuspendNowSwitchesTheNodeLinksOffAtOnce(t *testing.T) {
	// The row as SuspendNow leaves it; the fake does not apply its writes.
	svc, _, gw := nodeLinkService(&store.UserBilling{UserID: "u1", Status: "suspended", AdminHold: true})
	if err := svc.SuspendNow(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	want := []nodeLinkCall{{"link-node-a", false}, {"link-node-b", false}, {"enrolled-b", false}}
	if !reflect.DeepEqual(gw.calls, want) {
		t.Fatalf("hub calls %+v, want %+v", gw.calls, want)
	}
}

// The lifecycle suspends a tenant only while they are still past_due, decided
// in the write: a payment landing after it listed them used to be overwritten
// with a suspension nothing undid.
func TestTheLifecycleSuspendsOnlyFromPastDue(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	fs := &billingFakeStore{
		pastDue: []store.UserBilling{{UserID: "u1", Status: "past_due", GraceUntil: &past}},
		billing: &store.UserBilling{UserID: "u1", Status: "active"}, // the payment landed
	}
	svc := &BillingLifecycleService{store: fs, suspendGrace: 48 * time.Hour}
	svc.runOnce(context.Background())
	if len(fs.statusFrom) == 0 || !reflect.DeepEqual(fs.statusFrom[0], []string{"past_due"}) {
		t.Fatalf("the suspension was written from %v, want only from past_due", fs.statusFrom)
	}
}

// An operator's hold is enforced on the next pass, not after the payment grace:
// a server that came back up after it ran for two days.
func TestAnOperatorsHoldIsEnforcedWithoutTheGrace(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		hold      bool
		wantStops bool
	}{
		{"held", true, true},
		{"suspended for non-payment, still in grace", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &billingFakeStore{suspended: []store.UserBilling{
				{UserID: "u1", Status: "suspended", SuspendedAt: &now, AdminHold: tc.hold},
			}}
			svc := &BillingLifecycleService{store: fs, suspendGrace: 48 * time.Hour}
			svc.enforceSuspensions(context.Background())
			if stopped := len(fs.listServersCalls) > 0; stopped != tc.wantStops {
				t.Fatalf("servers stopped = %v, want %v", stopped, tc.wantStops)
			}
		})
	}
}

// Lifting a hold onto past_due gives the tenant their links back - past_due is
// not cut off - and a grace window when the dunning arrived during the hold.
func TestLiftingAHoldOntoPastDueRestoresTheLinksAndTheGrace(t *testing.T) {
	svc, fs, gw := nodeLinkService(&store.UserBilling{UserID: "u1", Status: "past_due", NodeLinksOff: true})
	if err := svc.HoldLifted("u1", "past_due"); err != nil {
		t.Fatal(err)
	}
	want := []nodeLinkCall{{"link-node-a", true}, {"link-node-b", true}, {"enrolled-b", true}}
	if !reflect.DeepEqual(gw.calls, want) {
		t.Fatalf("hub calls %+v, want %+v", gw.calls, want)
	}
	if len(fs.statusCalls) != 1 || fs.statusCalls[0].status != "past_due" || fs.statusCalls[0].graceUntil == nil {
		t.Fatalf("status writes %+v, want one past_due with a grace deadline", fs.statusCalls)
	}
}
