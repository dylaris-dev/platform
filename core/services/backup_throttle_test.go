package services

import (
	"context"
	"testing"

	"dylaris-core/models"
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
		"tok-a": `{"uplinkMbit":1000,"readMBps":null,"headroomPct":30,"saveOffMaxMin":null,"spoolEnabled":true,"spoolMaxGB":50}`,
		// A node with nothing set still gets the globals, for an env uplink.
		"tok-b": `{"uplinkMbit":null,"readMBps":null,"headroomPct":30,"saveOffMaxMin":null,"spoolEnabled":false,"spoolMaxGB":0}`,
	}
	for tok, w := range want {
		got, err := rdb.Get(context.Background(), NodeBackupPolicyKey(tok)).Result()
		if err != nil || got != w {
			t.Errorf("%s: got %q (%v), want %q", tok, got, err, w)
		}
	}
}
