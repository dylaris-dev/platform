package services

import (
	"dylaris-core/models"
	"dylaris-core/store"
)

// SettingBackupDefaultUserQuota is the platform's answer for a server owner who
// holds no entitlement: the operator-set allowance under Settings, Backups, on
// the platform limit convention (unset = no cap, "unlimited" = no cap, 0 = none,
// n = n GB).
//
// It replaces billing.r2_quota_gb, which asked the same question from the
// Billing screen. That was the wrong screen for it: the Billing tab is hidden
// without a hosted store, so on a self-hosted install the one control over
// every user's backup storage was invisible - while the guard behind it kept
// running. applyBackupAllowanceSettingMove carries an existing value across.
const SettingBackupDefaultUserQuota = "backup.default_user_quota_gb"

// backupAllowanceStore is the narrow surface the allowance needs. GetUserByID is
// here for the administrator check, and it asks about the SERVER OWNER rather
// than whoever pressed the button - an admin running a backup on a customer's
// server must meet the customer's ceiling, or the ceiling means nothing.
type backupAllowanceStore interface {
	GetUserByID(id string) (*models.User, error)
	GetUserBilling(userID string) (*store.UserBilling, error)
	GetSetting(key string) (string, error)
}

// BackupAllowanceGB resolves how much backup storage the PLATFORM will hold for
// one server owner, in GB. nil means no ceiling.
//
// One chain, four steps, and the first step that answers wins:
//
//	owner is an administrator   no ceiling, and nothing below is even read
//	per-user override           that number
//	entitlement (store on)      included x units, plus bookable if they consented
//	otherwise                   the operator's allowance under Settings, Backups
//
// The first two steps are what this platform was missing, and both already
// existed elsewhere: EffectiveEntitlement opens with the same administrator
// check ("An administrator is not a customer") and takes the same storeEnabled
// flag, and the over-limit sweep calls it exactly this way. The backup quota was
// the one gate that resolved a number by hand instead, so it was also the one
// gate an administrator could not get past on their own install, and the one
// that kept enforcing a commercial limit after the store was disconnected.
//
// storeEnabled gates the ENTITLEMENT step alone. Without a store nothing was
// ever bought, so the operator's own allowance is the only meaningful answer -
// which is what makes the Settings, Backups control work on a self-hosted
// install. A per-user override is still honoured there: an administrator typed
// that number by hand about one named user, and no global default is a more
// specific answer than that.
func BackupAllowanceGB(st backupAllowanceStore, ownerID string, storeEnabled bool) *int64 {
	if ownerID != "" {
		// A lookup FAILURE must not read as "not an admin" and silently apply a
		// customer ceiling to the operator, so treat an error as unknown and
		// fall through - the steps below are the same ones that ran before.
		if u, err := st.GetUserByID(ownerID); err == nil && u != nil && u.IsAdmin {
			return nil
		}
	}

	b, err := st.GetUserBilling(ownerID)
	if err != nil {
		// Same reasoning as above: an unreadable billing row is not a reason to
		// refuse a backup, and the operator allowance still applies.
		b = nil
	}
	if b != nil && b.R2QuotaGB != nil {
		return b.R2QuotaGB
	}
	if storeEnabled {
		if q := entitledR2QuotaGB(st, b); q != nil {
			return q
		}
	}
	raw, _ := st.GetSetting(SettingBackupDefaultUserQuota)
	return ParseLimitSetting(raw, nil)
}
