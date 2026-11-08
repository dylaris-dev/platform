package audit

import (
	"log"
	"net/http"

	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/store"
)

// Identity audit event types. Used by LogIdentity callers
// to keep event_type strings consistent. New event types
// (user_registered, login_failed, password_reset_requested, …)
// should be appended here so there's one canonical list.
const (
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

	// Every session of the account ended at once ("sign out everywhere").
	AuditEventSessionsRevoked = "sessions_revoked"

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
	// A role's capabilities are every holder's, so editing one changes access
	// for all of them at once; assigning it was audited, defining it was not.
	AuditEventPanelRoleCreated  = "panel_role_created"
	AuditEventPanelRoleUpdated  = "panel_role_updated"
	AuditEventPanelRoleDeleted  = "panel_role_deleted"
	AuditEventServerRoleCreated = "server_role_created"
	AuditEventServerRoleUpdated = "server_role_updated"
	AuditEventServerRoleDeleted = "server_role_deleted"
	// Actions an operator takes on an account that left no record before: an
	// admin setting someone's password is taking the account over, creating
	// one can create an admin, and a billing change can stop every server the
	// account owns.
	AuditEventUserCreated          = "user_created"
	AuditEventPasswordSetByAdmin   = "password_set_by_admin"
	AuditEventBillingStatusChanged = "billing_status_changed"
	AuditEventServerOwnerChanged   = "server_owner_changed"
	AuditEventMaintenanceToggled   = "maintenance_toggled"
	// An operator letting an account's API keys point gateway routes at public addresses.
	AuditEventGatewayPublicTargets = "gateway_public_targets_changed"
	// An operator taking a route-only route off the edges, or putting it back.
	AuditEventGatewayRouteSuspended   = "gateway_route_suspended"
	AuditEventGatewayRouteUnsuspended = "gateway_route_unsuspended"

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
	AuditEventStorageOrphansDeleted  = "storage_orphans_deleted"
	AuditEventStorageLifecycleSet    = "storage_lifecycle_applied"

	// API keys. A key acts like an invited member, so what it does is on the
	// record: its lifecycle, every refused request, every write on its owner's
	// realm, and its reads bundled per 10 minutes with a count.
	AuditEventAPIKeyCreated     = "api_key_created"
	AuditEventAPIKeyRevoked     = "api_key_revoked"
	AuditEventAPIKeyRolled      = "api_key_rolled"
	AuditEventAPIKeyDenied      = "api_key_denied"
	AuditEventAPIKeyUsed        = "api_key_used"
	AuditEventAPIKeyRead        = "api_key_read"
	AuditEventAPIKeyRateLimited = "api_key_rate_limited"
	// A key used by an account scheduled for auto-delete: in use, like a login.
	AuditEventDeletionCancelledByAPIKey = "deletion_cancelled_by_api_key"

	// Direct-mode DNS provider accounts. Metadata is the name and provider
	// only, never a credential.
	AuditEventDNSAccountCreated = "dns_account.created"
	AuditEventDNSAccountUpdated = "dns_account.updated"
	AuditEventDNSAccountDeleted = "dns_account.deleted"
	// Interval, limits and the feature flag of direct-mode DNS.
	AuditEventDNSSettingsUpdated = "dns_settings.updated"

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

// LogIdentity writes an append-only identity-domain audit row.
// Best-effort: errors are logged but never propagated — losing an audit row
// must never block the originating user action. Pass actorID = "" for system
// events (no human actor), targetID = "" when not applicable.
func LogIdentity(st store.Store, r *http.Request, eventType string, actorID, targetID string, metadata map[string]interface{}) {
	var key *models.APIKey
	if r != nil {
		key = reqctx.APIKeyFromContext(r)
	}
	LogIdentityAs(st, r, key, eventType, actorID, targetID, metadata)
}

// LogIdentityAs is LogIdentity with the acting key named explicitly
// (nil for none). It returns the new row's id, or 0 when nothing was written.
func LogIdentityAs(st store.Store, r *http.Request, key *models.APIKey, eventType string, actorID, targetID string, metadata map[string]interface{}) int64 {
	if st == nil {
		return 0
	}

	ev := &models.AuditEventIdentity{
		EventType: eventType,
		Metadata:  metadata,
	}
	if key != nil {
		id := key.ID
		ev.ActorAPIKeyID = &id
		ev.ActorAPIKeyName = key.Name
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
		ev.IPAddress = httpx.ClientIP(r)
		ev.UserAgent = r.UserAgent()
	}

	if err := st.InsertAuditIdentity(ev); err != nil {
		log.Printf("audit: failed to insert %s event: %v", eventType, err)
		return 0
	}
	return ev.ID
}
