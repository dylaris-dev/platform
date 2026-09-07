package services

import (
	"os"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// quotaFakeStore embeds store.Store (nil) so it satisfies the interface at
// compile time; only GetSetting is real. Any other call would panic, and these
// tests never make one.
type quotaFakeStore struct {
	store.Store
	kv      map[string]string
	billing *store.UserBilling
	user    *models.User
}

func (f *quotaFakeStore) GetSetting(key string) (string, error) { return f.kv[key], nil }

func (f *quotaFakeStore) GetUserBilling(string) (*store.UserBilling, error) {
	return f.billing, nil
}

func (f *quotaFakeStore) GetUserByID(string) (*models.User, error) { return f.user, nil }

const gb = int64(1024 * 1024 * 1024)

// The per-server node-local backup cap had no consumer at all: the settings
// form wrote backup.quota_per_server_gb and read it straight back, the Settings
// copy promised "Core checks current usage before approving a new run", and the
// Overview drew a usage bar with a Free figure - and nothing anywhere refused a
// run. MEASURED before the fix: cap 1 GB, 1.0 GB stored, Run Now accepted, card
// then read 1.2 GB / 1.0 GB.
func TestNodeLocalBackupQuota(t *testing.T) {
	srv := &models.Server{NodeID: 1, UUID: "srv-uuid"}

	cases := []struct {
		name         string
		kv           map[string]string
		used         int64
		usageKnown   bool
		wantExceeded bool
	}{
		{
			name:         "at the cap is refused, not just over it",
			kv:           map[string]string{"backup.mode": "node-local", "backup.quota_per_server_gb": "10"},
			used:         10 * gb,
			usageKnown:   true,
			wantExceeded: true,
		},
		{
			name:         "one byte under the cap still runs",
			kv:           map[string]string{"backup.mode": "node-local", "backup.quota_per_server_gb": "10"},
			used:         10*gb - 1,
			usageKnown:   true,
			wantExceeded: false,
		},
		{
			name:         "over the cap is refused",
			kv:           map[string]string{"backup.mode": "node-local", "backup.quota_per_server_gb": "1"},
			used:         gb + gb/5,
			usageKnown:   true,
			wantExceeded: true,
		},
		{
			// The cap counts a folder on the MC host. s3/shared archives are
			// bounded by the tenant R2 quota instead.
			name:         "another storage mode is not this cap's business",
			kv:           map[string]string{"backup.mode": "s3", "backup.quota_per_server_gb": "1"},
			used:         500 * gb,
			usageKnown:   true,
			wantExceeded: false,
		},
		{
			name:         "an unset mode means the shared default, so no cap",
			kv:           map[string]string{"backup.quota_per_server_gb": "1"},
			used:         500 * gb,
			usageKnown:   true,
			wantExceeded: false,
		},
		{
			// Documented as folded into the server's main disk quota - a
			// different budget. Charging the same bytes twice would be wrong.
			name: "sharing the quota with server storage hands it off",
			kv: map[string]string{
				"backup.mode": "node-local", "backup.quota_per_server_gb": "1",
				"backup.share_quota_with_server": "true",
			},
			used:         500 * gb,
			usageKnown:   true,
			wantExceeded: false,
		},
		{
			// A cap of 0 GB means node-local backups are not allowed at all. It
			// used to read as "unlimited", which made the one number meaning
			// "none" the number that switched the check off - the same inversion
			// the platform limit convention exists to remove.
			name:         "0 is a cap of NONE, so any usage is over",
			kv:           map[string]string{"backup.mode": "node-local", "backup.quota_per_server_gb": "0"},
			used:         500 * gb,
			usageKnown:   true,
			wantExceeded: true,
		},
		{
			// ...and it blocks the first byte, which is what a create gate asks.
			name:         "a cap of 0 blocks a server with nothing stored",
			kv:           map[string]string{"backup.mode": "node-local", "backup.quota_per_server_gb": "0"},
			used:         0,
			usageKnown:   true,
			wantExceeded: true,
		},
		{
			// The operator asking for no cap says so with the word, which is
			// what the settings form now writes.
			name:         "the unlimited sentinel means no cap",
			kv:           map[string]string{"backup.mode": "node-local", "backup.quota_per_server_gb": LimitUnlimited},
			used:         500 * gb,
			usageKnown:   true,
			wantExceeded: false,
		},
		{
			// Refusing a backup because the size could not be read would turn a
			// node blip into a skipped backup. The node has to be reachable to
			// run one anyway, so nothing is let through by failing open.
			name:         "unknown usage fails open",
			kv:           map[string]string{"backup.mode": "node-local", "backup.quota_per_server_gb": "1"},
			used:         0,
			usageKnown:   false,
			wantExceeded: false,
		},
		{
			// A missing row renders as the default in the settings GET, so the
			// check has to resolve it the same way or the bar and the refusal
			// disagree.
			name:         "a missing row uses the same default the form renders",
			kv:           map[string]string{"backup.mode": "node-local"},
			used:         int64(DefaultBackupQuotaPerServer) * gb,
			usageKnown:   true,
			wantExceeded: true,
		},
		{
			// Must not silently become unlimited.
			name:         "a malformed row falls back to the default, not to unlimited",
			kv:           map[string]string{"backup.mode": "node-local", "backup.quota_per_server_gb": "ten"},
			used:         int64(DefaultBackupQuotaPerServer) * gb,
			usageKnown:   true,
			wantExceeded: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &quotaFakeStore{kv: c.kv}
			exceeded, _, _ := nodeLocalBackupQuotaExceeded(st, srv, func() (int64, bool) {
				return c.used, c.usageKnown
			})
			if exceeded != c.wantExceeded {
				t.Errorf("exceeded = %v, want %v", exceeded, c.wantExceeded)
			}
		})
	}
}

// readSourceForQuotaCheck returns the file with full-line comments removed.
// Stripping them is the point: a source assertion will happily match the
// comment that explains the fix rather than the code doing it, which has caught
// me three times in these tests now.
func readSourceForQuotaCheck(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func containsCall(src, call string) bool { return strings.Contains(src, call) }

// Round 27 was the same shape one layer down: the scheduled executor did not
// take a gate the HTTP handler took. Both producers of a backup run must call
// this, so the check is on the call sites, not on a comment promising it.
func TestBothBackupProducersTakeTheQuota(t *testing.T) {
	for _, f := range []struct{ file, path string }{
		{"the manual/API path", "../handlers/backup.go"},
		{"the scheduler", "backup_scheduler.go"},
	} {
		t.Run(f.file, func(t *testing.T) {
			src := readSourceForQuotaCheck(t, f.path)
			if !containsCall(src, "NodeLocalBackupQuotaExceeded(") {
				t.Errorf("%s creates backup runs but never calls NodeLocalBackupQuotaExceeded; "+
					"a cap one producer skips is not a cap", f.path)
			}
		})
	}
}
