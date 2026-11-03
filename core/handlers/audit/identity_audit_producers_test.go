package audit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// identityEventPin is one identity audit event and every file that writes it.
// token is what the producer line contains: the constant's name, or the quoted
// wire string where the call site passes a literal.
type identityEventPin struct {
	event string
	token string
	files []string // relative to core/
}

// identityEventPins is the identity audit vocabulary as it is actually written.
// The server trail is pinned by TestEveryDeclaredAuditEventHasAProducer; the
// identity trail had no equivalent, so deleting the LogIdentity call for, say,
// a 2FA reset or an API key revocation left every test green while the record
// an incident review depends on quietly stopped being written.
//
// Removing a producer, moving it to a file not listed here, or renaming an
// event's wire string fails the test. Rows already in audit_events_identity
// keep their old string forever, so a rename is never a free refactor.
var identityEventPins = []identityEventPin{
	{"entitlement_granted", "AuditEventEntitlementGranted", []string{"handlers/billing/entitlement.go"}},
	{"entitlement_revoked", "AuditEventEntitlementRevoked", []string{"handlers/billing/entitlement.go"}},
	{"account_grant_assigned", "AuditEventAccountGrantAssigned", []string{"handlers/servers/server_grants.go"}},
	{"account_grant_revoked", "AuditEventAccountGrantRevoked", []string{"handlers/servers/server_grants.go"}},
	{"2fa_setup_completed", "AuditEvent2FASetupCompleted", []string{"handlers/auth/totp.go"}},
	{"2fa_disabled", "AuditEvent2FADisabled", []string{"handlers/auth/totp.go"}},
	{"2fa_admin_reset", "AuditEvent2FAAdminReset", []string{"handlers/auth/totp.go"}},
	{"2fa_backup_codes_regenerated", "AuditEvent2FABackupRegenerated", []string{"handlers/auth/totp.go"}},
	{"2fa_backup_code_consumed", "AuditEvent2FABackupCodeConsumed", []string{"handlers/auth/totp.go"}},
	{"sessions_revoked", "AuditEventSessionsRevoked", []string{"handlers/auth/auth.go"}},
	{"password_reset_requested", "AuditEventPasswordResetRequested", []string{"handlers/auth/password_reset.go"}},
	{"password_reset_completed", "AuditEventPasswordResetCompleted", []string{"handlers/auth/password_reset.go"}},
	{"security_questions_set", "AuditEventSecurityQuestionsSet", []string{"handlers/auth/security_questions.go"}},
	{"security_questions_pool_updated", "AuditEventSecurityQuestionsPoolUpdated", []string{"handlers/auth/security_questions.go"}},
	{"security_answers_failed_at_reset", "AuditEventSecurityAnswersFailedAtReset", []string{"handlers/auth/password_reset.go"}},
	{"deletion_warning_sent", `"deletion_warning_sent"`, []string{"services/auto_delete.go"}},
	{"user_anonymized", `"user_anonymized"`, []string{"services/auto_delete.go"}},
	{"user_hard_deleted", "AuditEventUserHardDeleted", []string{"handlers/users/users.go"}},
	{"deletion_cancelled_by_admin", "AuditEventDeletionCancelledByAdmin", []string{"handlers/users/users.go"}},
	{"deletion_cancelled_at_login", "AuditEventDeletionCancelledAtLogin", []string{"handlers/auth/auth.go"}},
	{"user_email_changed", "AuditEventUserEmailChanged", []string{"handlers/auth/auth.go", "handlers/auth/registration.go", "handlers/auth/user_email.go"}},
	{"user_role_changed", "AuditEventUserRoleChanged", []string{"handlers/users/user_role_perms.go"}},
	{"user_permissions_changed", "AuditEventUserPermissionsChanged", []string{"handlers/users/user_role_perms.go"}},
	{"user_panel_role_changed", "AuditEventUserPanelRoleChanged", []string{"handlers/users/user_role_perms.go"}},
	{"panel_role_created", "AuditEventPanelRoleCreated", []string{"handlers/settings/panel_roles.go"}},
	{"panel_role_updated", "AuditEventPanelRoleUpdated", []string{"handlers/settings/panel_roles.go"}},
	{"panel_role_deleted", "AuditEventPanelRoleDeleted", []string{"handlers/settings/panel_roles.go"}},
	{"server_role_created", "AuditEventServerRoleCreated", []string{"handlers/servers/server_roles.go"}},
	{"server_role_updated", "AuditEventServerRoleUpdated", []string{"handlers/servers/server_roles.go"}},
	{"server_role_deleted", "AuditEventServerRoleDeleted", []string{"handlers/servers/server_roles.go"}},
	{"user_created", "AuditEventUserCreated", []string{"handlers/users/users.go"}},
	{"password_set_by_admin", "AuditEventPasswordSetByAdmin", []string{"handlers/users/users.go"}},
	{"billing_status_changed", "AuditEventBillingStatusChanged", []string{"handlers/billing/billing.go"}},
	{"server_owner_changed", "AuditEventServerOwnerChanged", []string{"handlers/servers/servers_admin.go"}},
	{"maintenance_toggled", "AuditEventMaintenanceToggled", []string{"handlers/settings/maintenance.go"}},
	{"db_migration_started", "AuditEventDBMigrationStarted", []string{"handlers/admin/db_migration.go"}},
	{"db_hypertable_converted", "AuditEventDBHypertableConverted", []string{"handlers/admin/db_migration.go"}},
	{"storage_migration_started", "AuditEventStorageMigrationStart", []string{"handlers/storageapi/storage_migration.go"}},
	{"storage_migration_cancelled", "AuditEventStorageMigrationCancel", []string{"handlers/storageapi/storage_migration.go"}},
	{"storage_manifest_deleted", "AuditEventStorageManifestDeleted", []string{"handlers/storageapi/storage_migration.go"}},
	{"storage_orphans_deleted", "AuditEventStorageOrphansDeleted", []string{"handlers/backup/storage_orphans.go"}},
	{"storage_lifecycle_applied", "AuditEventStorageLifecycleSet", []string{"handlers/backup/storage_orphans.go"}},
	{"api_key_created", "AuditEventAPIKeyCreated", []string{"handlers/auth/api_keys.go"}},
	{"api_key_revoked", "AuditEventAPIKeyRevoked", []string{"handlers/auth/api_keys.go"}},
	{"api_key_denied", "AuditEventAPIKeyDenied", []string{"handlers/auth/api_key_audit.go"}},
	{"api_key_used", "AuditEventAPIKeyUsed", []string{"handlers/auth/api_key_audit.go"}},
	{"api_key_read", "AuditEventAPIKeyRead", []string{"handlers/auth/api_key_audit.go"}},
	{"api_key_rate_limited", "AuditEventAPIKeyRateLimited", []string{"handlers/auth/api_key_audit.go"}},
	{"deletion_cancelled_by_api_key", "AuditEventDeletionCancelledByAPIKey", []string{"handlers/auth/api_keys.go"}},

	// Written with a literal or a constant outside this package's catalog.
	{"user_registered", `"user_registered"`, []string{"handlers/auth/registration.go"}},
	{"email_verified", `"email_verified"`, []string{"handlers/auth/registration.go"}},
	{"user_email_change_requested", `"user_email_change_requested"`, []string{"handlers/auth/auth.go"}},
	{"ssh_key_added", "AuditEventSSHKeyAdded", []string{"handlers/users/ssh_keys.go"}},
	{"ssh_key_removed", "AuditEventSSHKeyRemoved", []string{"handlers/users/ssh_keys.go"}},
	{"ticket_restore_executed", `"ticket_restore_executed"`, []string{"handlers/tickets/ticket_migration.go"}},
	{"node.admission_changed", `"node.admission_changed"`, []string{"handlers/nodes/node_admission.go"}},
	{"node.pairing_reset", `"node.pairing_reset"`, []string{"handlers/nodes/node_admission.go"}},
	{"node.secret_rolled", `"node.secret_rolled"`, []string{"handlers/nodes/node_admission.go"}},
	{"node.join_approved", `"node.join_approved"`, []string{"handlers/nodes/node_admission.go", "handlers/nodes/node_self_service.go"}},
}

// identityNoProducer: declared in the catalog, written by nothing. A known gap,
// kept visible here rather than silently passing: region changes are not on
// the identity record today.
var identityNoProducer = map[string]string{
	"AuditEventRegionCreated":        "region CRUD does not audit yet",
	"AuditEventRegionUpdated":        "region CRUD does not audit yet",
	"AuditEventRegionDeleted":        "region CRUD does not audit yet",
	"AuditEventUserRegionsChanged":   "user region assignment does not audit yet",
	"AuditEventUserAllRegionsToggle": "user region assignment does not audit yet",
}

// declaration matches `Name = "value"` (or `Name = value`) at the start of a
// line, which is a definition, not a producer.
var declaration = regexp.MustCompile(`^\s*\w+\s*=`)

// mentions reports whether line references token as a whole word, so
// AuditEventAPIKeyDenied is not satisfied by ServerAuditEventAPIKeyDenied.
func mentions(line, token string) bool {
	if !strings.Contains(line, token) || declaration.MatchString(line) {
		return false
	}
	return regexp.MustCompile(`(^|\W)` + regexp.QuoteMeta(token) + `(\W|$)`).MatchString(line)
}

func producerLines(t *testing.T, rel, token string) int {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	n := 0
	for _, line := range strings.Split(string(body), "\n") {
		if mentions(line, token) {
			n++
		}
	}
	return n
}

func TestIdentityAuditEventsArePinnedToTheirProducers(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range identityEventPins {
		if seen[p.event] {
			t.Errorf("%s pinned twice", p.event)
		}
		seen[p.event] = true
		for _, f := range p.files {
			if producerLines(t, f, p.token) == 0 {
				t.Errorf("%s: %s no longer writes it (looked for %s) - the identity log silently stopped recording this", p.event, f, p.token)
			}
		}
	}
}

// Every constant in the identity catalog is pinned with its exact wire string,
// or listed as having no producer. A renamed string, or a new constant nobody
// pinned, fails here.
func TestIdentityAuditCatalogMatchesThePins(t *testing.T) {
	src, err := os.ReadFile("audit.go")
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	catalog := regexp.MustCompile(`(?m)^\s*(AuditEvent\w+)\s*=\s*"([^"]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(catalog) == 0 {
		t.Fatal("no AuditEvent constants found in audit.go - did the catalog move?")
	}
	byToken := map[string]string{}
	for _, p := range identityEventPins {
		byToken[p.token] = p.event
	}
	for _, m := range catalog {
		name, value := m[1], m[2]
		if reason, ok := identityNoProducer[name]; ok {
			if n := countUses(t, name); n > 0 {
				t.Errorf("%s is listed as unproduced (%s) but is used %d time(s) now - pin it instead", name, reason, n)
			}
			continue
		}
		pinned, ok := byToken[name]
		if !ok {
			// A catalog constant may be written via its literal elsewhere.
			pinned, ok = byToken[`"`+value+`"`]
		}
		switch {
		case !ok:
			t.Errorf("%s = %q is in the catalog but not pinned in identityEventPins", name, value)
		case pinned != value:
			t.Errorf("%s changed its wire string from %q to %q; rows already written keep the old one", name, pinned, value)
		}
	}
}

// countUses counts non-declaration references to name in non-test core code.
func countUses(t *testing.T, name string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(filepath.Join("..", ".."), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, line := range strings.Split(string(body), "\n") {
			if mentions(line, name) {
				n++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return n
}
