package services

import (
	"context"
	"dylaris-core/pkg/leader"
	"dylaris-core/store"
	"log"
	"strconv"
	"strings"
	"time"
)

// TicketAuditRetentionService runs a daily sweep that deletes ticket_audit_events
// older than the configured retention, mirroring ServerAuditRetentionService.
//
// The tickets.audit_retention_days setting has been settable, validated, stored
// and displayed since the Ticket module shipped, and nothing ever read it: the
// Ticket Settings tab offered "Audit retention - days" and an operator who
// lowered it to satisfy a data-minimisation rule got no pruning at all, while
// ticket_audit_events grew for the lifetime of the install. The table's own
// schema comment asserts that "the audience and retention concerns differ" from
// the identity audit - the retention half of that sentence was never built.
//
// An UNSET setting skips the sweep rather than falling back to the tab's
// suggested value. The suggestion is what an admin sees prefilled, not a policy
// they chose, and this sweep deletes: it enforces a retention horizon that was
// actually saved, never one nobody confirmed. An explicit 0 means keep forever.
//
// Leader-gated like every other periodic service so multi-Core does not fan out
// the DELETE.
type TicketAuditRetentionService struct {
	store    store.Store
	interval time.Duration
	leader   leader.Election
}

func NewTicketAuditRetentionService(s store.Store) *TicketAuditRetentionService {
	return &TicketAuditRetentionService{store: s, interval: leaderSweepInterval}
}

func (s *TicketAuditRetentionService) SetLeader(l leader.Election) { s.leader = l }

func (s *TicketAuditRetentionService) Start(ctx context.Context) {
	log.Printf("Ticket audit retention service started (interval: %s)", s.interval)
	s.runOnce(ctx)
	ticker := time.NewTicker(s.interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runOnce(ctx)
			}
		}
	}()
}

func (s *TicketAuditRetentionService) runOnce(_ context.Context) {
	if s.leader != nil && !s.leader.IsLeader() {
		return
	}
	daysStr, _ := s.store.GetSetting("tickets.audit_retention_days")
	// Atoi, not Sscanf("%d"): Sscanf stops at the first non-digit and reports
	// success, so "30d" would parse as 30 and this sweep would delete on a
	// setting it could not actually read.
	days, _ := strconv.Atoi(strings.TrimSpace(daysStr))
	if days <= 0 {
		// 0 = keep forever, and an unset or unparseable setting leaves days at 0
		// too - all of them mean "no confirmed horizon", so nothing is deleted.
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	deleted, err := s.store.PurgeTicketAuditOlderThan(cutoff)
	if err != nil {
		logErrf("ticket-audit-retention", "purge error: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("ticket-audit-retention: purged %d event(s) older than %d days", deleted, days)
	}
}
