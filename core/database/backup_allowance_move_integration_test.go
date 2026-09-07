package database

import (
	"database/sql"
	"testing"
)

// The allowance moved off the Billing screen, and the value has to move with it
// or every install that had set one silently loses its cap.
//
// The "0" case is the one that matters most and is deliberately NOT a carry: the
// old Billing GET answered "0" for an unset quota and its PUT stored whatever
// came back, so opening that screen once and pressing Save wrote a platform-wide
// refusal. MEASURED in this deployment before the move - billing.r2_quota_gb
// held "0" and every backup for an owner without an entitlement was refused.
// Carrying that across would ship a bug forward as policy.
func TestBackupAllowanceSettingMove(t *testing.T) {
	db := freshSchemaDB(t) // skips unless DYLARIS_TEST_DB_HOST is set

	const (
		oldKey = "billing.r2_quota_gb"
		newKey = "backup.default_user_quota_gb"
	)

	read := func(t *testing.T, key string) (string, bool) {
		t.Helper()
		var v string
		switch err := db.QueryRow(`SELECT value FROM settings WHERE key = $1`, key).Scan(&v); err {
		case sql.ErrNoRows:
			return "", false
		case nil:
			return v, true
		default:
			t.Fatalf("read %s: %v", key, err)
			return "", false
		}
	}
	set := func(t *testing.T, key, value string) {
		t.Helper()
		if _, err := db.Exec(
			`INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, NOW())
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}
	clear := func(t *testing.T) {
		t.Helper()
		if _, err := db.Exec(`DELETE FROM settings WHERE key IN ($1, $2)`, oldKey, newKey); err != nil {
			t.Fatalf("clear: %v", err)
		}
	}

	tests := []struct {
		name           string
		old, new       string // "" means the row is absent
		wantNew        string
		wantNewPresent bool
	}{
		{name: "a real cap moves across", old: "250", wantNew: "250", wantNewPresent: true},
		{name: "a decided no-cap moves across", old: "unlimited", wantNew: "unlimited", wantNewPresent: true},
		// The screen defect, not an operator's answer.
		{name: "a zero is dropped rather than carried", old: "0", wantNewPresent: false},
		{name: "nothing to move leaves nothing behind", wantNewPresent: false},
		// A value decided since the move outranks whatever the old key says.
		{name: "an existing new value is never overwritten", old: "250", new: "10", wantNew: "10", wantNewPresent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clear(t)
			if tt.old != "" {
				set(t, oldKey, tt.old)
			}
			if tt.new != "" {
				set(t, newKey, tt.new)
			}

			if err := applyBackupAllowanceSettingMove(db); err != nil {
				t.Fatalf("migration: %v", err)
			}

			got, present := read(t, newKey)
			if present != tt.wantNewPresent {
				t.Fatalf("%s present = %v (%q), want %v", newKey, present, got, tt.wantNewPresent)
			}
			if present && got != tt.wantNew {
				t.Errorf("%s = %q, want %q", newKey, got, tt.wantNew)
			}
			// The old row always goes. Leaving it would be a second source of
			// truth for one number, which is the shape that produced the defect.
			if _, stillThere := read(t, oldKey); stillThere {
				t.Errorf("%s survived the move", oldKey)
			}

			// Runs on every boot, so a second pass must change nothing.
			if err := applyBackupAllowanceSettingMove(db); err != nil {
				t.Fatalf("second run: %v", err)
			}
			again, presentAgain := read(t, newKey)
			if presentAgain != present || again != got {
				t.Errorf("second run changed %s from (%q,%v) to (%q,%v)", newKey, got, present, again, presentAgain)
			}
		})
	}
}
