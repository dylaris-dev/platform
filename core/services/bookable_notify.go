package services

import (
	"fmt"
	"log"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// Telling a tenant when the ceiling on their own bill moves.
//
// Metered billing is a consent to a KNOWN maximum: the customer is shown "you
// can book up to N GB more and it stops there" before they agree. An operator
// raising N later raises what that customer can be charged, and lowering it can
// stop them at a number they never saw. Either way the figure they agreed to is
// no longer the figure in force, so the ones who agreed are told.
//
// Only them. A tenant with metered billing off is not being charged and their
// stop is the included allowance, which this does not touch - a notification to
// them would be an alarm about a number that does not apply to them.

// NotifyTypeBookableChanged is the notification type for both, so a reader can
// filter the whole class. Producers and consumers share the vocabulary in
// handlers/notifications.go; this one lives here because services is where it is
// raised and handlers already imports services.
const NotifyTypeBookableChanged = "billing_bookable_changed"

// bookableNotifier is the slice of the store this needs. Narrow so a test can
// supply it without standing up everything a Store answers.
type bookableNotifier interface {
	ListUserBilling() ([]store.UserBilling, error)
	GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error)
	InsertNotification(n *models.Notification) (int64, error)
}

// NotifyBackupBookableChanged tells every tenant who agreed to be charged for
// backup storage that the maximum they may book has moved. Returns how many were
// notified, which is what the caller logs.
//
// perUnit values, because that is what the setting holds; each tenant is told
// THEIR number, the per-unit figure times what they hold. A tenant holding
// nothing has no bookable storage either way and is skipped - the change is
// real, but for them it is a change from zero to zero.
func NotifyBackupBookableChanged(st bookableNotifier, beforePerUnit, afterPerUnit int64) int {
	if beforePerUnit == afterPerUnit {
		return 0
	}
	rows, err := st.ListUserBilling()
	if err != nil {
		log.Printf("bookable: could not list tenants to notify about the backup ceiling: %v", err)
		return 0
	}
	sent := 0
	for i := range rows {
		b := &rows[i]
		if !b.BackupBillingEnabled {
			continue
		}
		units := backupUnits(b, time.Now())
		if units == 0 {
			continue
		}
		before, after := beforePerUnit*units, afterPerUnit*units
		if before == after {
			continue
		}
		if notify(st, b.UserID, "Bookable backup storage changed", fmt.Sprintf(
			"You have metered backup storage on. The most you can book on top of what your "+
				"subscription includes changed from %d GB to %d GB. New backups stop there, so "+
				"this is also the most it can cost you.", before, after), "") {
			sent++
		}
	}
	return sent
}

// NotifyTrafficPurchaseChanged does the same for one traffic allowance.
//
// scope is the row that changed: a user's own row concerns exactly that user,
// and the tenant default concerns everyone who does NOT have their own row for
// this (region, kind) - because for them the default is what answers. Asking per
// tenant is a query each; with the number of tenants a platform has that is
// cheaper than the alternative, which is caching a table an operator edits.
func NotifyTrafficPurchaseChanged(st bookableNotifier, scope, region, kind string, before, after *int64) int {
	if sameLimit(before, after) {
		return 0
	}
	rows, err := st.ListUserBilling()
	if err != nil {
		log.Printf("bookable: could not list tenants to notify about the %s/%s ceiling: %v", region, kind, err)
		return 0
	}
	ownScope := strings.HasPrefix(scope, "user:")
	target := strings.TrimPrefix(scope, "user:")

	sent := 0
	for i := range rows {
		b := &rows[i]
		if !b.TrafficBillingEnabled {
			continue
		}
		if ownScope {
			if b.UserID != target {
				continue
			}
		} else {
			// The tenant default only reaches tenants the resolver would ask it
			// about. One with their own row is unaffected, and telling them
			// their ceiling moved when it did not is worse than silence.
			own, err := st.GetTrafficLimit("user:"+b.UserID, region, kind)
			if err != nil {
				log.Printf("bookable: could not check %s's own %s/%s row: %v", b.UserID, region, kind, err)
				continue
			}
			if own != nil {
				continue
			}
		}
		units := backupUnits(b, time.Now())
		if units == 0 {
			continue
		}
		body := fmt.Sprintf(
			"You have metered traffic on. The most you can book on top of your %s allowance "+
				"changed from %s to %s. Traffic stops there rather than being billed further.",
			trafficPoolLabel(region, kind), bookableWords(before, units), bookableWords(after, units))
		if notify(st, b.UserID, "Bookable traffic changed", body, "/nodes") {
			sent++
		}
	}
	return sent
}

// notify writes one row, and never fails the operator's save: the setting IS
// changed by the time this runs, so a failed notification is a tenant who was
// not told rather than a change that did not happen. Loud in the log, because
// "why was I charged more than I agreed to" is answered by this row.
func notify(st bookableNotifier, userID, title, body, link string) bool {
	if userID == "" {
		return false
	}
	if _, err := st.InsertNotification(&models.Notification{
		UserID: userID,
		Type:   NotifyTypeBookableChanged,
		Title:  title,
		Body:   body,
		Link:   link,
	}); err != nil {
		log.Printf("bookable: could not notify %s about %q: %v", userID, title, err)
		return false
	}
	return true
}

// bookableWords renders a per-unit cap as the tenant's own number. nil is no cap
// at all, which is the one state that cannot be written as a quantity - and the
// one a customer most needs told apart from a large number.
func bookableWords(perUnit *int64, units int64) string {
	if perUnit == nil {
		return "no limit"
	}
	return fmt.Sprintf("%d GB", *perUnit*units)
}

// trafficPoolLabel names the pool the way the customer's own screens do. A raw
// "edge" or a literal "*" in front of a tenant means nothing to them.
func trafficPoolLabel(region, kind string) string {
	name := kind
	switch kind {
	case TrafficKindEdge:
		name = "player traffic"
	case TrafficKindRelay:
		name = "file transfer"
	}
	if region == TrafficRegionAny || region == "" {
		return name
	}
	return name + " (" + region + ")"
}

func sameLimit(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
