package store

import (
	"testing"
	"time"
)

// TestOwnerCutOff pins OwnerCutOff, which MUST stay equivalent to
// store.ownerCutOffSQL so LinkBoot, the warp enroll gate, the billing
// enforcement pass and the ACL reconciler all agree on when a tenant is actually
// cut off. They cut different things - the link's credential, the tunnel itself,
// the servers - and a disagreement between them is a tenant who is half
// suspended, or a link the reconciler restores every 60 seconds.
func TestOwnerCutOff(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	const grace = 48 * time.Hour
	const overGrace = 72 * time.Hour

	cases := []struct {
		name string
		b    *UserBilling
		want bool
	}{
		{"nil billing row is never cut off", nil, false},
		{"active status alone is never cut off", &UserBilling{Status: "active"}, false},
		{"past_due status alone is never cut off", &UserBilling{Status: "past_due"}, false},
		{
			"suspended but nil SuspendedAt is never cut off",
			&UserBilling{Status: "suspended", SuspendedAt: nil},
			false,
		},
		{
			"suspended and still within the grace window",
			&UserBilling{Status: "suspended", SuspendedAt: timePtr(now.Add(-1 * time.Hour))},
			false,
		},
		{
			// An operator's hold has no grace: it waited 48 hours like a missed
			// payment, with links and overlay admitted after a fraud suspension.
			"held by an operator is cut off at once",
			&UserBilling{Status: "suspended", AdminHold: true, SuspendedAt: timePtr(now.Add(-1 * time.Minute))},
			true,
		},
		{
			"suspended and exactly at the grace boundary (suspended_at+grace == now) is cut off",
			&UserBilling{Status: "suspended", SuspendedAt: timePtr(now.Add(-grace))},
			true,
		},
		{
			"suspended and past the grace window",
			&UserBilling{Status: "suspended", SuspendedAt: timePtr(now.Add(-grace - time.Hour))},
			true,
		},
		{
			"suspended one second short of the grace boundary is NOT cut off",
			&UserBilling{Status: "suspended", SuspendedAt: timePtr(now.Add(-grace + time.Second))},
			false,
		},
		// The over-limit arm. It used to be absent, and its absence was the
		// whole reason the over-limit cutoff never held: the sweep dropped the
		// warp peers, this gate let the client straight back in, and for a
		// route-only tenant - no servers, no warp - nothing happened at all.
		{
			"over its limits and past that grace is cut off, even while paying",
			&UserBilling{Status: "active", OverLimitSince: timePtr(now.Add(-overGrace - time.Hour))},
			true,
		},
		{
			"over its limits but still inside that grace is not cut off",
			&UserBilling{Status: "active", OverLimitSince: timePtr(now.Add(-1 * time.Hour))},
			false,
		},
		{
			"exactly at the over-limit boundary is cut off",
			&UserBilling{Status: "active", OverLimitSince: timePtr(now.Add(-overGrace))},
			true,
		},
		// The two graces run on separate clocks, so one being unfinished must
		// not excuse the other.
		{
			"inside the payment grace but past the over-limit one is still cut off",
			&UserBilling{
				Status:         "suspended",
				SuspendedAt:    timePtr(now.Add(-1 * time.Hour)),
				OverLimitSince: timePtr(now.Add(-overGrace - time.Hour)),
			},
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := OwnerCutOff(c.b, grace, overGrace, now); got != c.want {
				t.Errorf("OwnerCutOff() = %v, want %v", got, c.want)
			}
		})
	}
}

func timePtr(t time.Time) *time.Time { return &t }
