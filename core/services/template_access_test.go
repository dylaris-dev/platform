package services

import (
	"testing"

	"dylaris-core/models"
)

func TestTemplateAccess(t *testing.T) {
	personal := models.Template{OwnerID: "own", Visibility: "personal"}
	adminT := models.Template{OwnerID: "own", Visibility: "admin"}
	platform := models.Template{OwnerID: "adm", Visibility: "platform"}
	all := TemplateSettings{Audience: "all"}

	tests := []struct {
		name             string
		t                models.Template
		v                TemplateViewer
		s                TemplateSettings
		see, edit, share bool
	}{
		{"owner own personal", personal, TemplateViewer{UserID: "own"}, all, true, true, true},
		{"stranger personal", personal, TemplateViewer{UserID: "x"}, all, false, false, false},
		{"use sharer", personal, TemplateViewer{UserID: "x", ShareLevel: "use"}, all, true, false, false},
		{"edit sharer", personal, TemplateViewer{UserID: "x", ShareLevel: "edit"}, all, true, true, false},
		{"admin personal", personal, TemplateViewer{UserID: "a", IsAdmin: true}, all, true, true, true},
		{"admin-read sees admin tpl", adminT, TemplateViewer{UserID: "x", HasAdminRead: true}, all, true, false, false},
		{"owner of admin tpl, no role", adminT, TemplateViewer{UserID: "own"}, all, false, false, false},
		{"share ignored on admin tpl", adminT, TemplateViewer{UserID: "x", ShareLevel: "edit"}, all, false, false, false},
		{"admin edits admin tpl", adminT, TemplateViewer{UserID: "a", IsAdmin: true}, all, true, true, false},
		{"platform hidden from users", platform, TemplateViewer{UserID: "x"}, all, false, false, false},
		{"platform visible when enabled", platform, TemplateViewer{UserID: "x"}, TemplateSettings{Audience: "all", PlatformForUsers: true}, true, false, false},
		{"platform owner non-admin cannot edit", platform, TemplateViewer{UserID: "adm"}, TemplateSettings{Audience: "all", PlatformForUsers: true}, true, false, false},
		{"admin platform", platform, TemplateViewer{UserID: "a", IsAdmin: true}, all, true, true, false},
		{"admin sees all, audience off", personal, TemplateViewer{UserID: "a", IsAdmin: true}, TemplateSettings{Audience: "off"}, true, true, true},
		{"user nothing, audience admin", personal, TemplateViewer{UserID: "own"}, TemplateSettings{Audience: "admin"}, false, true, true},
		{"user nothing, audience off", platform, TemplateViewer{UserID: "x"}, TemplateSettings{Audience: "off", PlatformForUsers: true}, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TemplateCanSee(tc.t, tc.v, tc.s); got != tc.see {
				t.Errorf("see = %v, want %v", got, tc.see)
			}
			if got := TemplateCanEdit(tc.t, tc.v); got != tc.edit {
				t.Errorf("edit = %v, want %v", got, tc.edit)
			}
			if got := TemplateCanShare(tc.t, tc.v); got != tc.share {
				t.Errorf("share = %v, want %v", got, tc.share)
			}
		})
	}

	create := []struct {
		name, vis string
		v         TemplateViewer
		s         TemplateSettings
		want      bool
	}{
		{"user personal open", "personal", TemplateViewer{}, all, true},
		{"user personal needs role", "personal", TemplateViewer{}, TemplateSettings{Audience: "all", CreateRequiresRole: true}, false},
		{"user personal with role", "personal", TemplateViewer{HasCreate: true}, TemplateSettings{Audience: "all", CreateRequiresRole: true}, true},
		{"user personal audience admin", "personal", TemplateViewer{}, TemplateSettings{Audience: "admin"}, false},
		{"admin personal audience off", "personal", TemplateViewer{IsAdmin: true}, TemplateSettings{Audience: "off", CreateRequiresRole: true}, true},
		{"user platform", "platform", TemplateViewer{HasCreate: true}, all, false},
		{"user admin", "admin", TemplateViewer{HasAdminRead: true}, all, false},
		{"admin platform", "platform", TemplateViewer{IsAdmin: true}, all, true},
		{"admin admin", "admin", TemplateViewer{IsAdmin: true}, all, true},
		{"unknown visibility", "bogus", TemplateViewer{IsAdmin: true}, all, false},
	}
	for _, tc := range create {
		t.Run("create "+tc.name, func(t *testing.T) {
			if got := TemplateCanCreate(tc.vis, tc.v, tc.s); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
