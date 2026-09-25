package handlers

import (
	"log"
	"net/http"

	"dylaris-core/models"
)

// Identity audit event types. Used by LogIdentityAudit callers
// to keep event_type strings consistent. New event types
// (user_registered, login_failed, password_reset_requested, …)
// should be appended here so there's one canonical list.
const (
	AuditEventRegionCreated        = "region_created"
	AuditEventRegionUpdated        = "region_updated"
	AuditEventRegionDeleted        = "region_deleted"
	AuditEventUserRegionsChanged   = "user_regions_changed"
	AuditEventUserAllRegionsToggle = "user_all_regions_toggled"

	// Manual entitlement grants. Audited because a grant hands out paid capability
	// for free, with an expiry an operator will not remember setting - "who gave
	// this tenant BYON, when, and for how long" has to be answerable later.
	AuditEventEntitlementGranted = "entitlement_granted"
	AuditEventEntitlementRevoked = "entitlement_revoked"

	// Account-wide grants: one person handed access to EVERY server in a realm,
	// present and future. The per-server case belongs in that server's own audit
	// log; this one has no single server to belong to, and is the more powerful
	// of the two, so it lands here rather than nowhere.
	AuditEventAccountGrantAssigned = "account_grant_assigned"
	AuditEventAccountGrantRevoked  = "account_grant_revoked"

	// 2FA / TOTP lifecycle. The "consumed" event distinguishes
	// successful backup-code uses (rare, expected to drop ownership counts)
	// from regular TOTP success (noisy, intentionally not audited).
	AuditEvent2FASetupCompleted     = "2fa_setup_completed"
	AuditEvent2FADisabled           = "2fa_disabled"
	AuditEvent2FAAdminReset         = "2fa_admin_reset"
	AuditEvent2FABackupRegenerated  = "2fa_backup_codes_regenerated"
	AuditEvent2FABackupCodeConsumed = "2fa_backup_code_consumed"

	// Password reset flow.
	AuditEventPasswordResetRequested = "password_reset_requested"
	AuditEventPasswordResetCompleted = "password_reset_completed"

	// Security questions.
	AuditEventSecurityQuestionsSet         = "security_questions_set"
	AuditEventSecurityQuestionsPoolUpdated = "security_questions_pool_updated"
	AuditEventSecurityAnswersFailedAtReset = "security_answers_failed_at_reset"

	// Auto-delete of inactive users.
	AuditEventDeletionWarningSent      = "deletion_warning_sent"
	AuditEventUserAnonymized           = "user_anonymized"
	AuditEventUserHardDeleted          = "user_hard_deleted"
	AuditEventDeletionCancelledByAdmin = "deletion_cancelled_by_admin"
	AuditEventDeletionCancelledAtLogin = "deletion_cancelled_at_login"

	// Roles + maintenance.
	AuditEventUserEmailChanged       = "user_email_changed"
	AuditEventUserRoleChanged        = "user_role_changed"
	AuditEventUserPermissionsChanged = "user_permissions_changed"
	AuditEventUserPanelRoleChanged   = "user_panel_role_changed"
	AuditEventMaintenanceToggled     = "maintenance_toggled"

	// Moving the platform's own data. These used to be filed under
	// maintenance_toggled, all five of them, with the real action buried in a
	// metadata field - so the identity log answered "somebody moved the entire
	// platform database to another host" with "Maintenance toggled". An audit
	// trail that names the wrong noun is worse than one that says nothing,
	// which is the same reason runtime_changed exists beside resources_changed
	// in the server trail.
	AuditEventDBMigrationStarted     = "db_migration_started"
	AuditEventDBHypertableConverted  = "db_hypertable_converted"
	AuditEventStorageMigrationStart  = "storage_migration_started"
	AuditEventStorageMigrationCancel = "storage_migration_cancelled"
	AuditEventStorageManifestDeleted = "storage_manifest_deleted"

	// Tickets (ticket-scoped audit, written to ticket_audit_events
	// rather than audit_events_identity).
	TicketEventCreated         = "ticket_created"
	TicketEventStatusChanged   = "status_changed"
	TicketEventPriorityChanged = "priority_changed"
	TicketEventAssigned        = "assigned"
	TicketEventUnassigned      = "unassigned"
	TicketEventWatcherAdded    = "watcher_added"
	TicketEventWatcherRemoved  = "watcher_removed"
	TicketEventReopened        = "reopened"

	// Attachments + auto-close.
	TicketEventAttachmentAdded   = "attachment_added"
	TicketEventAttachmentRemoved = "attachment_removed"
	TicketEventAutoClosed        = "auto_closed"
)

// LogIdentityAudit writes an append-only identity-domain audit row.
// Best-effort: errors are logged but never propagated — losing an audit row
// must never block the originating user action. Pass actorID = "" for system
// events (no human actor), targetID = "" when not applicable.
func LogIdentityAudit(state *AppState, r *http.Request, eventType string, actorID, targetID string, metadata map[string]interface{}) {
	if state == nil || state.Store == nil {
		return
	}

	ev := &models.AuditEventIdentity{
		EventType: eventType,
		Metadata:  metadata,
	}
	if actorID != "" {
		a := actorID
		ev.ActorUserID = &a
	}
	if targetID != "" {
		t := targetID
		ev.TargetUserID = &t
	}
	if r != nil {
		ev.IPAddress = clientIP(r)
		ev.UserAgent = r.UserAgent()
	}

	if err := state.Store.InsertAuditIdentity(ev); err != nil {
		log.Printf("audit: failed to insert %s event: %v", eventType, err)
	}
}
