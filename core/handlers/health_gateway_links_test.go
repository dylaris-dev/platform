package handlers

import "testing"

// A link REGISTRATION outlives the link. link:<token> is written with a 24 h
// TTL, online_link:<token> with 15 s, so a token that stops being used sits in
// the registration list for up to a day - after a link is redeployed under a
// new token, after a kit is revoked, after a customer switches their box off.
//
// Production was reading "degraded" because of exactly that: two registrations,
// one link service, everything working. The page has to be worth reading, so an
// offline registration is reported and not graded; what IS graded is the state
// that actually takes servers off the air, which is routes with no link at all.
func TestGatewayHealthDoesNotGradeALeftoverLinkRegistration(t *testing.T) {
	cases := []struct {
		name                                             string
		onlineEdges, totalEdges, onlineLinks, totalLinks int
		routes                                           int64
		wantStatus, wantLinkStatus                       string
	}{
		{
			// The measured production state.
			name:        "a stale registration beside a working link is not a fault",
			onlineEdges: 2, totalEdges: 2, onlineLinks: 1, totalLinks: 2, routes: 1,
			wantStatus: "up", wantLinkStatus: "up",
		},
		{
			name:        "no link at all while routes exist is a real outage",
			onlineEdges: 2, totalEdges: 2, onlineLinks: 0, totalLinks: 2, routes: 1,
			wantStatus: "down", wantLinkStatus: "down",
		},
		{
			// Nothing is being carried, so nothing is broken - this is a
			// platform that has not been used for routing yet.
			name:        "no link and no routes is not an outage",
			onlineEdges: 2, totalEdges: 2, onlineLinks: 0, totalLinks: 2, routes: 0,
			wantStatus: "up", wantLinkStatus: "up",
		},
		{
			name:        "an edge down still degrades",
			onlineEdges: 1, totalEdges: 2, onlineLinks: 1, totalLinks: 1, routes: 1,
			wantStatus: "degraded", wantLinkStatus: "up",
		},
		{
			name:        "no edge at all is the loudest state and wins",
			onlineEdges: 0, totalEdges: 2, onlineLinks: 0, totalLinks: 1, routes: 1,
			wantStatus: "down", wantLinkStatus: "down",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, detail, reason, linkStatus := gatewayVerdict(
				c.onlineEdges, c.totalEdges, c.onlineLinks, c.totalLinks, c.routes)
			if status != c.wantStatus {
				t.Errorf("status = %q, want %q (detail %q, reason %q)", status, c.wantStatus, detail, reason)
			}
			if linkStatus != c.wantLinkStatus {
				t.Errorf("link row = %q, want %q", linkStatus, c.wantLinkStatus)
			}
			if status != "up" && reason == "" {
				t.Errorf("status %q carries no reason; the page has to say what to do", status)
			}
		})
	}
}
