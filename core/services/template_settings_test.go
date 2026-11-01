package services

import (
	"context"
	"testing"
)

type tplSettingsStore map[string]string

func (s tplSettingsStore) GetSetting(key string) (string, error) { return s[key], nil }

func TestLoadTemplateSettings(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		chk  func(TemplateSettings) bool
	}{
		{"defaults", nil, func(s TemplateSettings) bool {
			return s.Audience == "off" && s.MaxPerUser != nil && *s.MaxPerUser == 10 &&
				s.MaxSizeBytes != nil && *s.MaxSizeBytes == 512<<20 && s.ContentPolicy == "no-jars" &&
				s.MaxSizeBytesAdmin != nil && *s.MaxSizeBytesAdmin == 2048<<20 &&
				!s.PlatformForUsers && !s.CreateRequiresRole && s.StorageConnectionID == 0
		}},
		{"unlimited count", map[string]string{"templates_max_per_user": "unlimited"}, func(s TemplateSettings) bool { return s.MaxPerUser == nil }},
		{"zero count", map[string]string{"templates_max_per_user": "0"}, func(s TemplateSettings) bool { return s.MaxPerUser != nil && *s.MaxPerUser == 0 }},
		{"size in MB", map[string]string{"templates_max_size_mb": "2"}, func(s TemplateSettings) bool { return s.MaxSizeBytes != nil && *s.MaxSizeBytes == 2097152 }},
		{"unlimited size", map[string]string{"templates_max_size_mb": "unlimited"}, func(s TemplateSettings) bool { return s.MaxSizeBytes == nil }},
		{"1 TiB size", map[string]string{"templates_max_size_mb": "1048576"}, func(s TemplateSettings) bool { return s.MaxSizeBytes != nil && *s.MaxSizeBytes == 1<<40 }},
		{"oversized size reads as default", map[string]string{"templates_max_size_mb": "9223372036854775807"}, func(s TemplateSettings) bool {
			return s.MaxSizeBytes != nil && *s.MaxSizeBytes == 512<<20
		}},
		{"admin size in MB", map[string]string{"templates_max_size_mb_admin": "3"}, func(s TemplateSettings) bool {
			return s.MaxSizeBytesAdmin != nil && *s.MaxSizeBytesAdmin == 3<<20 && *s.MaxSizeBytes == 512<<20
		}},
		{"unlimited admin size", map[string]string{"templates_max_size_mb_admin": "unlimited"}, func(s TemplateSettings) bool { return s.MaxSizeBytesAdmin == nil }},
		{"oversized admin size reads as default", map[string]string{"templates_max_size_mb_admin": "9223372036854775807"}, func(s TemplateSettings) bool {
			return s.MaxSizeBytesAdmin != nil && *s.MaxSizeBytesAdmin == 2048<<20
		}},
		{"policy uploads", map[string]string{"templates_content_policy": "uploads"}, func(s TemplateSettings) bool { return s.ContentPolicy == "uploads" }},
		{"policy garbage", map[string]string{"templates_content_policy": "garbage"}, func(s TemplateSettings) bool { return s.ContentPolicy == "no-jars" }},
		{"bad storage id", map[string]string{"templates_storage_connection_id": "x"}, func(s TemplateSettings) bool { return s.StorageConnectionID == 0 }},
		{"storage id", map[string]string{"templates_storage_connection_id": "7"}, func(s TemplateSettings) bool { return s.StorageConnectionID == 7 }},
		{"flags", map[string]string{"feature_templates": "all", "templates_platform_for_users": "true", "templates_create_requires_role": "true"}, func(s TemplateSettings) bool {
			return s.Audience == "all" && s.PlatformForUsers && s.CreateRequiresRole
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ff := NewFeatureFlags(tplSettingsStore(tc.in))
			if s := LoadTemplateSettings(context.Background(), ff); !tc.chk(s) {
				t.Fatalf("unexpected settings: %+v", s)
			}
		})
	}
}

func TestTemplateSizeCap(t *testing.T) {
	s := TemplateSettings{MaxSizeBytes: LimitPtr(1), MaxSizeBytesAdmin: LimitPtr(2)}
	tests := []struct {
		name       string
		visibility string
		ownerAdmin bool
		own        bool
		want       *int64
	}{
		{"user personal", "personal", false, false, s.MaxSizeBytes},
		{"admin owner personal", "personal", true, false, s.MaxSizeBytesAdmin},
		{"admin visibility", "admin", false, false, s.MaxSizeBytesAdmin},
		{"platform visibility", "platform", false, false, s.MaxSizeBytesAdmin},
		{"user on own storage", "personal", false, true, nil},
		{"admin on own storage", "personal", true, true, nil},
	}
	for _, tc := range tests {
		if got := s.SizeCap(tc.visibility, tc.ownerAdmin, tc.own); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
