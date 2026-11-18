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

// ServerAuditRetentionService runs a daily sweep that deletes server_audit_events
// older than the configured retention. Honors the audit.server_retention_days
// setting; 0, unset and unparseable all mean "keep forever" and skip the sweep.
// Because it deletes, it only ever applies a horizon an operator actually saved,
// never the value the settings card happens to prefill.
//
// Leader-gated like all other periodic services so multi-Core doesn't fan out
// the DELETE.
type ServerAuditRetentionService struct {
	store    store.Store
	interval time.Duration
	leader   leader.Election
}

func NewServerAuditRetentionService(s store.Store) *ServerAuditRetentionService {
	return &ServerAuditRetentionService{store: s, interval: leaderSweepInterval}
}

func (s *ServerAuditRetentionService) SetLeader(l leader.Election) { s.leader = l }

func (s *ServerAuditRetentionService) Start(ctx context.Context) {
	log.Printf("Server audit retention service started (interval: %s)", s.interval)
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

func (s *ServerAuditRetentionService) runOnce(_ context.Context) {
	if s.leader != nil && !s.leader.IsLeader() {
		return
	}
	daysStr, _ := s.store.GetSetting("audit.server_retention_days")
	// Atoi, not Sscanf("%d"): Sscanf stops at the first non-digit and reports
	// success, so "30d" would parse as 30 and this sweep would delete on a
	// setting it could not actually read.
	days, _ := strconv.Atoi(strings.TrimSpace(daysStr))
	if days <= 0 {
		// 0 = unlimited retention, and so is unset or unparseable: this sweep
		// only ever applies a horizon that was really saved.
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	deleted, err := s.store.PurgeServerAuditOlderThan(cutoff)
	if err != nil {
		logErrf("server-audit-retention", "purge error: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("server-audit-retention: purged %d events older than %d days", deleted, days)
	}
}
