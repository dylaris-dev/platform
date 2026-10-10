package backup

import (
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func ruleIDs(rules []types.LifecycleRule) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = aws.ToString(r.ID)
	}
	return out
}

func TestMergeLifecycleRules(t *testing.T) {
	foreign := types.LifecycleRule{
		ID: aws.String("customer-logs"), Status: types.ExpirationStatusEnabled,
		Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("logs/")},
		Expiration: &types.LifecycleExpiration{Days: aws.Int32(30)},
	}
	// R2 ships every bucket with a default abort rule under its own id; it is
	// not ours and must survive.
	r2Default := types.LifecycleRule{
		ID: aws.String("Default Multipart Abort Rule"), Status: types.ExpirationStatusEnabled,
		AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(7)},
	}
	staleOurs := types.LifecycleRule{
		ID: aws.String(LifecycleRuleAbortMultipart), Status: types.ExpirationStatusEnabled,
		AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(9)},
	}
	ours := DylarisLifecycleRules("pfx/", 2)

	cases := []struct {
		name     string
		existing []types.LifecycleRule
		want     []string
	}{
		{"empty bucket", nil, []string{LifecycleRuleAbortMultipart, LifecycleRuleMigrationExpiry}},
		{"keeps foreign rules in order", []types.LifecycleRule{foreign, r2Default},
			[]string{"customer-logs", "Default Multipart Abort Rule", LifecycleRuleAbortMultipart, LifecycleRuleMigrationExpiry}},
		{"replaces an old copy of ours", []types.LifecycleRule{staleOurs, foreign},
			[]string{"customer-logs", LifecycleRuleAbortMultipart, LifecycleRuleMigrationExpiry}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeLifecycleRules(tc.existing, ours)
			if !reflect.DeepEqual(ruleIDs(got), tc.want) {
				t.Fatalf("ids = %v, want %v", ruleIDs(got), tc.want)
			}
			for _, r := range got {
				if aws.ToString(r.ID) == LifecycleRuleAbortMultipart && aws.ToInt32(r.AbortIncompleteMultipartUpload.DaysAfterInitiation) != 3 {
					t.Fatal("the stale copy of our rule survived the merge")
				}
				if aws.ToString(r.ID) == "customer-logs" && !reflect.DeepEqual(r, foreign) {
					t.Fatal("a foreign rule was altered")
				}
			}
			// Idempotent: a second apply gives the same document.
			if again := MergeLifecycleRules(got, ours); !reflect.DeepEqual(again, got) {
				t.Fatalf("second merge differs: %v vs %v", ruleIDs(again), ruleIDs(got))
			}
		})
	}
}

func TestDylarisLifecycleRules(t *testing.T) {
	root := DylarisLifecycleRules("", 0)
	if len(root) != 1 || aws.ToString(root[0].Filter.Prefix) != "" {
		t.Fatalf("a storage at the bucket root wants one bucket-wide rule, got %v", ruleIDs(root))
	}
	mt := DylarisLifecycleRules("server-backups/", 3)
	if len(mt) != 2 {
		t.Fatalf("got %v", ruleIDs(mt))
	}
	if p := aws.ToString(mt[0].Filter.Prefix); p != "server-backups/" {
		t.Fatalf("abort rule prefix = %q; it must stay inside the storage's own folder", p)
	}
	if p := aws.ToString(mt[1].Filter.Prefix); p != "server-backups/migration-transfer/" {
		t.Fatalf("migration expiry prefix = %q; it must stay inside the storage's own folder", p)
	}
	if d := aws.ToInt32(mt[1].Expiration.Days); d != 3 {
		t.Fatalf("migration expiry = %d days, want 3", d)
	}
}

func TestMigrationExpiryDays(t *testing.T) {
	cases := []struct {
		ttl  time.Duration
		want int32
	}{
		{0, 2},
		{6 * time.Hour, 2}, // the BYON default
		{24 * time.Hour, 2},
		{25 * time.Hour, 3},
		{48 * time.Hour, 3},
		{72*time.Hour + time.Minute, 5},
	}
	for _, tc := range cases {
		if got := MigrationExpiryDays(tc.ttl); got != tc.want {
			t.Errorf("MigrationExpiryDays(%v) = %d, want %d", tc.ttl, got, tc.want)
		}
	}
}
