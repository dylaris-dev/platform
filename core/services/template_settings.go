package services

import (
	"context"
	"strconv"
)

// Settings keys for the Templates feature. The audience key is TemplatesSettingKey.
const (
	TemplatesSettingKey            = "feature_templates"
	TemplatesMaxPerUserKey         = "templates_max_per_user"
	TemplatesMaxSizeMBKey          = "templates_max_size_mb"
	TemplatesContentPolicyKey      = "templates_content_policy"
	TemplatesPlatformForUsersKey   = "templates_platform_for_users"
	TemplatesCreateRequiresRoleKey = "templates_create_requires_role"
	TemplatesStorageConnectionKey  = "templates_storage_connection_id"
)

// TemplatesMaxSizeMBCap is the largest size limit an operator may set, 1 TiB.
// The value is shifted into bytes, so an unbounded one wraps int64 into a
// negative or tiny cap.
const TemplatesMaxSizeMBCap = 1 << 20

// Content policies: under no-jars a template holds no .jar at all.
const (
	TemplatePolicyNoJars  = "no-jars"
	TemplatePolicyUploads = "uploads"
)

// TemplateSettings is every operator knob of the Templates feature, resolved.
// MaxPerUser and MaxSizeBytes follow the limits convention: nil is no cap, 0 is
// none. StorageConnectionID 0 means Core storage.
type TemplateSettings struct {
	Audience            string
	MaxPerUser          *int
	MaxSizeBytes        *int64
	ContentPolicy       string
	PlatformForUsers    bool
	CreateRequiresRole  bool
	StorageConnectionID int
}

// LoadTemplateSettings reads the settings, applying the product defaults
// (10 templates per user, 512 MiB each, no-jars).
func LoadTemplateSettings(ctx context.Context, ff *FeatureFlags) TemplateSettings {
	s := TemplateSettings{
		Audience:           ff.TemplatesAudience(ctx),
		ContentPolicy:      TemplatePolicyNoJars,
		PlatformForUsers:   ff.Get(ctx, TemplatesPlatformForUsersKey, false),
		CreateRequiresRole: ff.Get(ctx, TemplatesCreateRequiresRoleKey, false),
	}
	if n := ParseLimitSetting(ff.GetString(ctx, TemplatesMaxPerUserKey, ""), LimitPtr(10)); n != nil {
		v := int(*n)
		s.MaxPerUser = &v
	}
	mb := ParseLimitSetting(ff.GetString(ctx, TemplatesMaxSizeMBKey, ""), LimitPtr(512))
	if mb != nil && *mb > TemplatesMaxSizeMBCap {
		// Stored before the PUT bounded it; the default beats a wrapped number.
		mb = LimitPtr(512)
	}
	if mb != nil {
		v := *mb << 20
		s.MaxSizeBytes = &v
	}
	if ff.GetString(ctx, TemplatesContentPolicyKey, "") == TemplatePolicyUploads {
		s.ContentPolicy = TemplatePolicyUploads
	}
	if id, err := strconv.Atoi(ff.GetString(ctx, TemplatesStorageConnectionKey, "")); err == nil && id > 0 {
		s.StorageConnectionID = id
	}
	return s
}
