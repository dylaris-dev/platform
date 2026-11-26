package services

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/DATA-DOG/go-sqlmock"
)

func p64(v int64) *int64 { return &v }

func TestNodeBackupThrottleValidate(t *testing.T) {
	tests := []struct {
		name string
		in   NodeBackupThrottle
		ok   bool
	}{
		{"all unset", NodeBackupThrottle{}, true},
		{"uplink and read set", NodeBackupThrottle{UplinkMbit: p64(1000), ReadMBps: p64(40)}, true},
		// A zero rate would hang the run with saving off: not a cap.
		{"zero uplink refused", NodeBackupThrottle{UplinkMbit: p64(0)}, false},
		{"zero read refused", NodeBackupThrottle{ReadMBps: p64(0)}, false},
		{"absurd uplink refused", NodeBackupThrottle{UplinkMbit: p64(maxUplinkMbit + 1)}, false},
		{"spool needs a size", NodeBackupThrottle{SpoolEnabled: true}, false},
		{"spool with size", NodeBackupThrottle{SpoolEnabled: true, SpoolMaxGB: 50}, true},
		{"negative size refused", NodeBackupThrottle{SpoolMaxGB: -1}, false},
	}
	for _, tt := range tests {
		if err := tt.in.Validate(); (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}

func TestBackupThrottleGlobals(t *testing.T) {
	tests := []struct {
		name         string
		values       map[string]string
		wantHeadroom int64
		wantCeiling  *int64
	}{
		{"never saved: defaults", map[string]string{}, 30, p64(20)},
		{"saved", map[string]string{SettingBackupThrottleHeadroomPct: "50", SettingBackupThrottleSaveOffMax: "45"}, 50, p64(45)},
		{"no ceiling", map[string]string{SettingBackupThrottleSaveOffMax: LimitUnlimited}, 30, nil},
		{"ceiling 0", map[string]string{SettingBackupThrottleSaveOffMax: "0"}, 30, p64(0)},
		{"headroom out of range keeps the default", map[string]string{SettingBackupThrottleHeadroomPct: "95"}, 30, p64(20)},
	}
	for _, tt := range tests {
		h, c := BackupThrottleGlobals(&settingsStub{values: tt.values})
		if h != tt.wantHeadroom || (c == nil) != (tt.wantCeiling == nil) || (c != nil && *c != *tt.wantCeiling) {
			t.Errorf("%s: got %d %v, want %d %v", tt.name, h, c, tt.wantHeadroom, tt.wantCeiling)
		}
	}
}

// The node parses this exact shape (node/backup_throttle.go); the field names
// are a cross-module contract.
func TestPublishBackupPoliciesWireShape(t *testing.T) {
	rdb := newQueueTestRedis(t)
	st := &settingsStub{
		values: map[string]string{SettingBackupThrottleSaveOffMax: LimitUnlimited},
		nodes:  []models.Node{{ID: 1, Token: "tok-a"}, {ID: 2, Token: "tok-b"}, {ID: 3}},
	}
	if err := SaveNodeBackupThrottle(st, 1, NodeBackupThrottle{UplinkMbit: p64(1000), SpoolEnabled: true, SpoolMaxGB: 50}); err != nil {
		t.Fatal(err)
	}
	PublishBackupPolicies(context.Background(), st, rdb)

	want := map[string]string{
		"tok-a": `{"uplinkMbit":1000,"readMBps":null,"headroomPct":30,"saveOffMaxMin":null,"spoolEnabled":true,"spoolMaxGB":50,"shareQuotaWithServer":false}`,
		// A node with nothing set still gets the globals, for an env uplink.
		"tok-b": `{"uplinkMbit":null,"readMBps":null,"headroomPct":30,"saveOffMaxMin":null,"spoolEnabled":false,"spoolMaxGB":0,"shareQuotaWithServer":false}`,
	}
	for tok, w := range want {
		got, err := rdb.Get(context.Background(), NodeBackupPolicyKey(tok)).Result()
		if err != nil || got != w {
			t.Errorf("%s: got %q (%v), want %q", tok, got, err, w)
		}
	}
}

// R54-02/R54-04: the share mode rides on the policy every node gets, so a
// node without backups learns a change. R54-03: an unreadable setting leaves
// the field out (the node keeps its mode) instead of sending false; R56-06:
// and flags it, so the node does not take it for an older Core.
func TestPublishBackupPoliciesCarriesTheShareMode(t *testing.T) {
	tests := []struct {
		name string
		st   *settingsStub
		want string
	}{
		{"never saved: separate budget", &settingsStub{}, `"shareQuotaWithServer":false`},
		{"shared", &settingsStub{values: map[string]string{"backup.share_quota_with_server": "true"}}, `"shareQuotaWithServer":true`},
		{"unreadable: no field", &settingsStub{errs: map[string]error{"backup.share_quota_with_server": errors.New("timeout")}}, ""},
	}
	for _, tt := range tests {
		rdb := newQueueTestRedis(t)
		tt.st.nodes = []models.Node{{ID: 1, Token: "tok-a"}}
		PublishBackupPolicies(context.Background(), tt.st, rdb)
		got, err := rdb.Get(context.Background(), NodeBackupPolicyKey("tok-a")).Result()
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if tt.want == "" {
			if strings.Contains(got, "shareQuotaWithServer") || !strings.Contains(got, `"shareQuotaUnreadable":true`) {
				t.Errorf("%s: got %s, want no share field and the unreadable flag", tt.name, got)
			}
		} else if !strings.Contains(got, tt.want) || strings.Contains(got, "shareQuotaUnreadable") {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
}

// R57-03: a failed settings read must not go out as "nothing set". The
// node stores whatever parses, so an empty throttle switched pacing and the
// spool off. The last published policy stays instead.
func TestPublishBackupPoliciesKeepsTheLastPolicyOnAFailedRead(t *testing.T) {
	const old = `{"uplinkMbit":1000,"spoolEnabled":true,"spoolMaxGB":50}`
	boom := errors.New("timeout")
	tests := []struct {
		name  string
		errs  map[string]error
		keepA bool // node 1's policy is left alone
		keepB bool // node 2's policy is left alone
	}{
		{"node throttle unreadable", map[string]error{NodeBackupThrottleSetting(1): boom}, true, false},
		{"headroom unreadable", map[string]error{SettingBackupThrottleHeadroomPct: boom}, true, true},
		{"ceiling unreadable", map[string]error{SettingBackupThrottleSaveOffMax: boom}, true, true},
		{"all readable", nil, false, false},
	}
	for _, tt := range tests {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()
		st := &settingsStub{errs: tt.errs, nodes: []models.Node{{ID: 1, Token: "tok-a"}, {ID: 2, Token: "tok-b"}}}
		rdb.Set(ctx, NodeBackupPolicyKey("tok-a"), old, 0)
		rdb.Set(ctx, NodeBackupPolicyKey("tok-b"), old, 0)
		PublishBackupPolicies(ctx, st, rdb)
		for tok, keep := range map[string]bool{"tok-a": tt.keepA, "tok-b": tt.keepB} {
			got, _ := rdb.Get(ctx, NodeBackupPolicyKey(tok)).Result()
			if (got == old) != keep {
				t.Errorf("%s: %s = %s, want kept=%v", tt.name, tok, got, keep)
			}
		}
		if err := PublishBackupPolicy(ctx, st, rdb, models.Node{ID: 1, Token: "tok-a"}); (err != nil) != tt.keepA {
			t.Errorf("%s: PublishBackupPolicy err = %v, want an error=%v", tt.name, err, tt.keepA)
		}
	}
}

// R57-02: the tick runs on every replica and overwrites Redis, so it must
// not republish a one-second-old snapshot over a save another replica just
// published (the share mode re-tags every backup folder on the node).
func TestPublishBackupPoliciesReadsPastTheSettingsSnapshot(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := store.NewPostgresStore(db)
	q := regexp.QuoteMeta(`SELECT key, value FROM settings`)
	mock.ExpectQuery(q).WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).
		AddRow("backup.share_quota_with_server", "false").AddRow(SettingBackupThrottleHeadroomPct, "30"))
	if _, err := st.GetSetting("backup.share_quota_with_server"); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`FROM nodes`)).WillReturnRows(sqlmock.NewRows(make([]string, 26)).
		AddRow(1, "n1", "addr", "tok-a", "online", false, "", false, 0, "", "", time.Now(), "", "[]", nil,
			1.0, 1.0, 0, 0, "", false, nil, "", "", nil, nil))
	// Another replica saved these and published them.
	mock.ExpectQuery(q).WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).
		AddRow("backup.share_quota_with_server", "true").AddRow(SettingBackupThrottleHeadroomPct, "60"))

	PublishBackupPolicies(ctx, st, rdb)

	got, err := rdb.Get(ctx, NodeBackupPolicyKey("tok-a")).Result()
	if err != nil || !strings.Contains(got, `"shareQuotaWithServer":true`) || !strings.Contains(got, `"headroomPct":60`) {
		t.Errorf("policy = %s (%v): the stale snapshot was republished", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
