package backup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Bucket lifecycle rules Core manages on the platform's OWN buckets.
//
// Ours are told apart from everyone else's by the ID prefix alone. A bucket's
// lifecycle configuration is ONE document, and a PUT replaces all of it, so a
// rule we did not write is read back and written back, never dropped.
//
// Known limit of that round trip: it goes through the SDK's types, so an XML
// element of a foreign rule the SDK does not model is not part of what is
// written back. Every option the SDK knows survives; an exotic provider
// extension on a rule managed elsewhere may not.
const (
	LifecycleIDPrefix            = "dylaris-"
	LifecycleRuleAbortMultipart  = "dylaris-abort-mpu"
	LifecycleRuleMigrationExpiry = "dylaris-migration-transfer"

	// MigrationTransferDir is where the cross-LAN migration path parks its
	// temporary archive (services.transferViaR2).
	MigrationTransferDir = "migration-transfer/"
)

// ErrLifecycleForbidden means the storage's credentials may not read or change
// the bucket's lifecycle configuration. On R2 that is every token below "Admin
// Read & Write": object-scoped tokens cannot touch bucket configuration.
var ErrLifecycleForbidden = errors.New("these credentials are not allowed to manage the bucket's lifecycle rules")

// KeyPrefix is the storage's folder inside the bucket with a trailing slash,
// or "" for the bucket root - the form a lifecycle Filter.Prefix needs.
func (s *S3Storage) KeyPrefix() string {
	if s.prefix == "" {
		return ""
	}
	return s.prefix + "/"
}

// Bucket is the bucket name, for display.
func (s *S3Storage) Bucket() string { return s.bucket }

// LifecycleRules returns the bucket's current rules. A bucket that has none is
// an empty list, not an error.
func (s *S3Storage) LifecycleRules(ctx context.Context) ([]types.LifecycleRule, error) {
	out, err := s.client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) && ae.ErrorCode() == "NoSuchLifecycleConfiguration" {
			return nil, nil
		}
		return nil, lifecycleErr("reading", err)
	}
	return out.Rules, nil
}

// PutLifecycleRules replaces the bucket's lifecycle configuration with rules.
// Callers pass the result of MergeLifecycleRules, never ours alone.
func (s *S3Storage) PutLifecycleRules(ctx context.Context, rules []types.LifecycleRule) error {
	_, err := s.client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket:                 aws.String(s.bucket),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: rules},
	})
	if err != nil {
		return lifecycleErr("writing", err)
	}
	return nil
}

func lifecycleErr(op string, err error) error {
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorCode() == "AccessDenied" {
		return fmt.Errorf("%s lifecycle rules: %w", op, ErrLifecycleForbidden)
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusForbidden {
		return fmt.Errorf("%s lifecycle rules: %w", op, ErrLifecycleForbidden)
	}
	return fmt.Errorf("%s lifecycle rules: %w", op, err)
}

const abortMultipartDays = 3

// MigrationExpiryDays is how long a migration transfer object may live: at
// least a day past the longest its presigned URLs stay valid, so the rule can
// never delete an archive a target is still allowed to download, and never
// under two days.
func MigrationExpiryDays(urlTTL time.Duration) int32 {
	days := int32((urlTTL+24*time.Hour-1)/(24*time.Hour)) + 1
	if days < 2 {
		days = 2
	}
	return days
}

// DylarisLifecycleRules is the set Core wants on one storage: abandoned
// multipart uploads under the storage's own folder aborted after three days
// (not one: a large BYON backup over a slow home line can legitimately upload
// for more than a day, and Core's own reaper already aborts stalled uploads
// after 6h), and - only on a storage that receives migration transfers, which
// migrationExpiryDays > 0 says - those temporary archives expired. keyPrefix
// is the storage's KeyPrefix; empty means the whole bucket.
func DylarisLifecycleRules(keyPrefix string, migrationExpiryDays int32) []types.LifecycleRule {
	rules := []types.LifecycleRule{{
		ID:                             aws.String(LifecycleRuleAbortMultipart),
		Status:                         types.ExpirationStatusEnabled,
		Filter:                         &types.LifecycleRuleFilter{Prefix: aws.String(keyPrefix)},
		AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(abortMultipartDays)},
	}}
	if migrationExpiryDays > 0 {
		rules = append(rules, types.LifecycleRule{
			ID:         aws.String(LifecycleRuleMigrationExpiry),
			Status:     types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String(keyPrefix + MigrationTransferDir)},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(migrationExpiryDays)},
		})
	}
	return rules
}

// MergeLifecycleRules keeps every rule of existing that is not ours, in its
// order, and appends ours. Applying it twice gives the same document, so the
// admin action can be pressed again without stacking duplicates.
func MergeLifecycleRules(existing, ours []types.LifecycleRule) []types.LifecycleRule {
	out := make([]types.LifecycleRule, 0, len(existing)+len(ours))
	for _, r := range existing {
		if IsDylarisLifecycleRule(r) {
			continue
		}
		out = append(out, r)
	}
	return append(out, ours...)
}

// IsDylarisLifecycleRule reports whether Core wrote the rule.
func IsDylarisLifecycleRule(r types.LifecycleRule) bool {
	return strings.HasPrefix(aws.ToString(r.ID), LifecycleIDPrefix)
}
