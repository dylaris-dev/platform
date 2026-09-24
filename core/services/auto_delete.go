package services

import (
	"context"
	"dylaris-core/mailer"
	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// AutoDeleteService runs a daily job that warns dormant users by email and
// later deletes (anonymizes by default) them according to the configured
// policy. Gated on the global Core leader so under multi-Core only one
// instance ever processes warnings/deletions per tick.
type AutoDeleteService struct {
	store       store.Store
	frontendURL string
	interval    time.Duration
	leader      leader.Election

	// Teardown deps, wired after the ACL provisioner and gateway exist. Until
	// they are, this sweep removes account ROWS and leaves what those accounts
	// ran - see SetLinkACL.
	gateway     GatewayProvider
	redis       *redis.Client
	provisioner *redisacl.Provisioner
	// warpPeers drops a departing account's overlay peers. Wired separately
	// (SetWarpPeers) because the warp service is built after this one; nil where
	// there is no overlay, and then the peers are left behind.
	warpPeers WarpPeerDisconnector
}

// SetWarpPeers wires the overlay teardown this sweep needs. Same shape and same
// name as the billing lifecycle's, for the same reason: the warp service does
// not exist yet when this one is constructed.
func (s *AutoDeleteService) SetWarpPeers(w WarpPeerDisconnector) { s.warpPeers = w }

// SetLeader wires the leader-election gate. Without it the service runs
// on every tick (single-Core dev mode).
func (s *AutoDeleteService) SetLeader(l leader.Election) { s.leader = l }

// SetLinkACL wires what this sweep needs to remove a tenant's infrastructure
// along with their account. Named and shaped like the billing lifecycle's
// setter of the same name because it is wired at the same moment, from the same
// three values, for the same reason: both are constructed before the ACL
// provisioner exists.
//
// Without it the sweep still runs. It deletes the account row and leaves the
// tenant's link kit's Redis credential, its tunnel key and its protected
// addresses in place, which is what it did before this was added.
func (s *AutoDeleteService) SetLinkACL(gw GatewayProvider, rdb *redis.Client, prov *redisacl.Provisioner) {
	s.gateway = gw
	s.redis = rdb
	s.provisioner = prov
}

// policySnapshot is what the service reads from the settings store on each
// tick. Mirrors a subset of handlers.AuthPolicy — duplicated here on purpose
// to avoid a services→handlers import cycle. The setting keys are the
// stable contract between the two.
type policySnapshot struct {
	Enabled          bool
	InactiveDays     int
	HistoryGraceDays int
	WarningDays      int
	Mode             string // "anonymize" | "hard_delete"
}

func NewAutoDeleteService(s store.Store, frontendURL string) *AutoDeleteService {
	return &AutoDeleteService{
		store:       s,
		frontendURL: frontendURL,
		interval:    24 * time.Hour,
	}
}

func (s *AutoDeleteService) Start(ctx context.Context) {
	log.Printf("Auto-delete service started (interval: %s)", s.interval)
	// First tick on startup so a freshly-deployed Core processes the queue
	// without waiting a full day. Then settle into the regular cadence.
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

func (s *AutoDeleteService) runOnce(ctx context.Context) {
	// Leader-gate first — cheap atomic load, avoids loading the policy at
	// all on followers and avoids any DB chatter from the runOnce body.
	if s.leader != nil && !s.leader.IsLeader() {
		return
	}
	policy := s.loadPolicy()
	if !policy.Enabled {
		return
	}
	// Stage A: send warnings + stamp scheduled date.
	s.processWarnings(ctx, policy)
	// Stage B: execute deletions whose scheduled date has arrived.
	s.processExecutions(ctx, policy)
}

func (s *AutoDeleteService) loadPolicy() policySnapshot {
	p := policySnapshot{
		Enabled:          false,
		InactiveDays:     90,
		HistoryGraceDays: 90,
		WarningDays:      7,
		Mode:             "anonymize",
	}
	if v, _ := s.store.GetSetting("auth.inactive_delete_enabled"); v != "" {
		p.Enabled = v == "true"
	}
	if v, _ := s.store.GetSetting("auth.inactive_days_before_delete"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 1 {
			p.InactiveDays = n
		}
	}
	if v, _ := s.store.GetSetting("auth.history_grace_extra_days"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			p.HistoryGraceDays = n
		}
	}
	if v, _ := s.store.GetSetting("auth.delete_email_warning_days"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			p.WarningDays = n
		}
	}
	if v, _ := s.store.GetSetting("auth.deletion_mode"); v == "hard_delete" || v == "anonymize" {
		p.Mode = v
	}
	return p
}

func (s *AutoDeleteService) processWarnings(_ context.Context, p policySnapshot) {
	idleSince := time.Now().Add(-time.Duration(p.InactiveDays) * 24 * time.Hour)
	candidates, err := s.store.ListInactiveCandidates(idleSince)
	if err != nil {
		log.Printf("auto-delete: list candidates: %v", err)
		return
	}
	now := time.Now()
	for _, c := range candidates {
		// History grace: skip until the extra window has also elapsed.
		if c.HasHistory && p.HistoryGraceDays > 0 {
			extraCutoff := now.Add(-time.Duration(p.InactiveDays+p.HistoryGraceDays) * 24 * time.Hour)
			lastActivity := c.LastLogin
			if lastActivity == nil {
				// Never logged in — fall back to a sentinel that always
				// looks recent (so an account that was just created and
				// never used doesn't get the extended grace unfairly).
				// Concretely: use the candidate's idle cutoff itself,
				// which guarantees the comparison is "older than idle,
				// older than extra cutoff?" — the user wouldn't be in
				// this list at all if they weren't past idle.
				idleStamp := idleSince
				lastActivity = &idleStamp
			}
			if lastActivity.After(extraCutoff) {
				continue
			}
		}

		scheduledAt := now.Add(time.Duration(p.WarningDays) * 24 * time.Hour)
		if err := s.store.MarkUserPendingDeletion(c.ID, scheduledAt); err != nil {
			log.Printf("auto-delete: mark pending for userID=%s: %v", c.ID, err)
			continue
		}

		// Best-effort warning mail. Failure doesn't roll back the stamp —
		// admin can manually cancel via the API if SMTP is misconfigured,
		// and a user who actually logs in cancels automatically.
		if c.Email != "" {
			if err := sendDeletionWarningEmail(s.store, s.frontendURL, c.Email, c.Username, scheduledAt, p.Mode); err != nil {
				logErrf("auto-delete", "warning mail to %s failed: %v", c.Email, err)
			}
		}

		insertAuditEvent(s.store, "deletion_warning_sent", c.ID, map[string]interface{}{
			"scheduled_at": scheduledAt.Format(time.RFC3339),
			"mode":         p.Mode,
			"has_history":  c.HasHistory,
		})
	}
}

func (s *AutoDeleteService) processExecutions(ctx context.Context, p policySnapshot) {
	due, err := s.store.ListUsersDueForDeletion(time.Now())
	if err != nil {
		log.Printf("auto-delete: list due: %v", err)
		return
	}
	for _, userID := range due {
		// Both modes, and this is not belt-and-braces: "anonymize" is the
		// DEFAULT, and it keeps the row. Without this the person is gone from
		// the panel while their link kit still authenticates against Redis and
		// their protected addresses still route - the account is what was
		// removed, not what the account ran.
		//
		// A failure here skips the account entirely rather than removing it
		// anyway. Leaving a dormant row for another day is recoverable; removing
		// the only record of who owned a live credential is not.
		if err := TeardownTenantInfrastructure(ctx, s.store, s.gateway, s.redis, s.provisioner, s.warpPeers, userID); err != nil {
			logErrf("auto-delete", "teardown for userID=%s failed, leaving the account in place: %v", userID, err)
			continue
		}
		// Read WHO this is before the row goes, because afterwards nothing can
		// say it. target_user_id is a foreign key onto users, so a row written
		// after a HARD delete cannot point at the account at all - the insert is
		// refused (23503) and insertAuditEvent swallows the error, so the row
		// never appears. ON DELETE SET NULL blanks the target of rows that
		// already exist; it does nothing for one that arrives afterwards.
		// Measured on production on the admin delete path, which does the same
		// thing one file over.
		//
		// The id, username and address therefore go in the metadata, and the
		// hard-delete branch passes no target at all. Anonymize keeps the row,
		// so it keeps its target.
		deleted := map[string]interface{}{"userId": userID}
		if u, uerr := s.store.GetUserByID(userID); uerr == nil && u != nil {
			deleted["username"] = u.Username
			deleted["email"] = u.Email
		}

		switch p.Mode {
		case "hard_delete":
			if err := s.store.DeleteUser(userID); err != nil {
				log.Printf("auto-delete: hard-delete userID=%s: %v", userID, err)
				continue
			}
			// No target: the user row is gone and the foreign key refuses a
			// reference to it. The metadata carries who it was.
			insertAuditEvent(s.store, "user_hard_deleted", "", deleted)
		default: // "anonymize"
			if err := s.store.AnonymizeUser(userID); err != nil {
				log.Printf("auto-delete: anonymize userID=%s: %v", userID, err)
				continue
			}
			insertAuditEvent(s.store, "user_anonymized", userID, deleted)
		}
	}
}

// insertAuditEvent is a tiny wrapper so the service doesn't have to depend
// on the handlers package just for one helper. Errors are swallowed: an
// audit insert failure is logged but never blocks the underlying action.
func insertAuditEvent(s store.Store, eventType string, targetUserID string, metadata map[string]interface{}) {
	ev := &models.AuditEventIdentity{
		EventType: eventType,
		Metadata:  metadata,
	}
	if targetUserID != "" {
		tid := targetUserID
		ev.TargetUserID = &tid
	}
	if err := s.InsertAuditIdentity(ev); err != nil {
		log.Printf("auto-delete: audit insert %s: %v", eventType, err)
	}
}

// sendDeletionWarningEmail composes and sends the warning. Uses the "auth"
// SMTP purpose with the usual fallback to "default" — same as registration
// and password-reset.
func sendDeletionWarningEmail(s store.Store, frontendURL, to, username string, scheduledAt time.Time, mode string) error {
	transport, err := mailer.Load(s, "auth")
	if err != nil {
		return fmt.Errorf("mail not configured: %w", err)
	}
	loginURL := strings.TrimRight(frontendURL, "/") + "/login"
	verb := "anonymized (account details wiped, identifier kept for audit references)"
	if mode == "hard_delete" {
		verb = "permanently deleted"
	}
	body := fmt.Sprintf(`Hi %s,

We noticed your Dylaris account has been inactive for some time. Per our retention policy, dormant accounts are %s.

Your account is scheduled for action on %s.

To keep your account, just sign in once before that date:

%s

If you do nothing, the action will go ahead automatically. No further reminders will be sent.

— Dylaris
`, username, verb, scheduledAt.UTC().Format("2006-01-02 15:04 UTC"), loginURL)

	_ = json.Marshal // silence unused if importer ever drops it
	return transport.Send(mailer.Message{
		To:      to,
		Subject: "Your Dylaris account is scheduled for deletion",
		Body:    body,
	})
}
