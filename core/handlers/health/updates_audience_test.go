package health

import (
	"context"
	"testing"
)

// A self-hosted platform has no customers, so the hosted notes (plans,
// storefront, BYON) must not reach a non-admin; an admin keeps the platform feed
// either way.
func TestAudienceNotes(t *testing.T) {
	cases := []struct {
		name     string
		hosted   bool
		isAdmin  bool
		wantFeed string
		wantAny  bool
	}{
		{"self-hosted user", false, false, feedHosted, false},
		{"self-hosted admin", false, true, feedPlatform, true},
		{"hosted user", true, false, feedHosted, true},
		{"hosted admin", true, true, feedPlatform, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &UpdatesHandler{d: Deps{Hosted: c.hosted}}
			feed, rel := h.audienceNotes(context.Background(), c.isAdmin)
			if feed != c.wantFeed {
				t.Errorf("feed = %q, want %q", feed, c.wantFeed)
			}
			if (len(rel) > 0) != c.wantAny {
				t.Errorf("got %d releases, want any=%v", len(rel), c.wantAny)
			}
		})
	}
}
