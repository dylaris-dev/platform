package services

import (
	"context"
	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/store"
	"fmt"
	"log"
	"time"
)

// TicketAutoCloseService runs a daily ticker that auto-closes resolved
// tickets that have been idle for the configured period. Honors the
// tickets.auto_close_enabled + tickets.auto_close_days_after_resolved
// settings; both are off by default so this is a no-op on fresh installs.
//
// Leader-gated like AutoDeleteService — without it, every Core would close
// the same tickets each tick under multi-instance.
type TicketAutoCloseService struct {
	store    store.Store
	interval time.Duration
	leader   leader.Election
}

func NewTicketAutoCloseService(s store.Store) *TicketAutoCloseService {
	return &TicketAutoCloseService{store: s, interval: leaderSweepInterval}
}

func (s *TicketAutoCloseService) SetLeader(l leader.Election) { s.leader = l }

func (s *TicketAutoCloseService) Start(ctx context.Context) {
	log.Printf("Ticket auto-close service started (interval: %s)", s.interval)
	s.runOnce(ctx) // immediate first pass
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

func (s *TicketAutoCloseService) runOnce(_ context.Context) {
	if s.leader != nil && !s.leader.IsLeader() {
		return
	}
	enabled, _ := s.store.GetSetting("tickets.auto_close_enabled")
	if enabled != "true" {
		return
	}
	daysStr, _ := s.store.GetSetting("tickets.auto_close_days_after_resolved")
	var days int
	fmt.Sscanf(daysStr, "%d", &days)
	if days < 1 {
		days = 7 // safety default if setting hasn't been written yet
	}

	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	ids, err := s.store.ListResolvedTicketsOlderThan(cutoff)
	if err != nil {
		logErrf("ticket-autoclose", "list error: %v", err)
		return
	}
	for _, id := range ids {
		if err := s.store.UpdateTicketStatus(id, "closed"); err != nil {
			logErrf("ticket-autoclose", "close %d failed: %v", id, err)
			continue
		}
		_ = s.store.InsertTicketAudit(&models.TicketAuditEvent{
			TicketID:  id,
			EventType: "auto_closed",
			Metadata: map[string]interface{}{
				"days_after_resolved": days,
			},
		})
	}
	if len(ids) > 0 {
		log.Printf("ticket-autoclose: closed %d ticket(s)", len(ids))
	}
}
