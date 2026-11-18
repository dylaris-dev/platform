package services

import (
	"testing"
	"time"
)

// The daily leader-gated sweeps start a fresh 24h ticker on every boot, and a
// replica that boots while the old leader still holds the lease skips its first
// pass. A Core redeployed more often than once a day therefore never ran them.
// Each sweep is idempotent, so ticking hourly is how they survive a deploy.
func TestLeaderGatedSweepsTickOftenEnoughToSurviveRedeploys(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
	}{
		{"auto-delete", NewAutoDeleteService(nil, "").interval},
		{"server audit retention", NewServerAuditRetentionService(nil).interval},
		{"ticket audit retention", NewTicketAuditRetentionService(nil).interval},
		{"ticket auto-close", NewTicketAutoCloseService(nil).interval},
	}
	for _, tc := range cases {
		if tc.interval > time.Hour {
			t.Errorf("%s ticks every %s; a Core redeployed within that never runs it", tc.name, tc.interval)
		}
	}
}
