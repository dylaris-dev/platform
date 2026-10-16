package handlers

import (
	"testing"
	"time"

	"dylaris-core/store"
)

// The store closes a month by asking for it after it ended; a future month is
// refused rather than read as an empty one.
func TestStoreUsagePeriod(t *testing.T) {
	now := time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC)
	tests := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"", "2026-10-01", true},
		{"2026-09", "2026-09-01", true},
		{"2026-10", "2026-10-01", true},
		{"2026-11", "", false},
		{"2026-9", "", false},
		{"september", "", false},
	}
	for _, tt := range tests {
		got, ok := storeUsagePeriod(tt.raw, now)
		if ok != tt.ok || (ok && got.Format("2006-01-02") != tt.want) {
			t.Errorf("storeUsagePeriod(%q) = %s, %v; want %s, %v", tt.raw, got.Format("2006-01-02"), ok, tt.want, tt.ok)
		}
	}
}

// An administrator's quota is space given: all of it is free and nothing past
// it can be booked, because Core stops the tenant there.
func TestAnAdminQuotaIsNotBilled(t *testing.T) {
	q := int64(200)
	included, bookable := billedBackupAllowance(nil, &store.UserBilling{R2QuotaGB: &q})
	if included != 200 || bookable != 0 {
		t.Fatalf("allowance = %d included, %d bookable; want 200 and 0", included, bookable)
	}
}
