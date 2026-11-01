package services

import "dylaris-core/models"

// TemplateViewer is who is asking, resolved by the handler from the request and
// the share tables. ShareLevel is "", "use" or "edit".
type TemplateViewer struct {
	UserID       string
	IsAdmin      bool
	HasAdminRead bool // templates.admin.read
	HasCreate    bool // templates.create
	ShareLevel   string
}

// TemplateCanSee: admins always; everyone else only while the audience is "all".
func TemplateCanSee(t models.Template, v TemplateViewer, s TemplateSettings) bool {
	if v.IsAdmin {
		return true
	}
	if s.Audience != "all" {
		return false
	}
	switch t.Visibility {
	case "personal":
		return t.OwnerID == v.UserID || v.ShareLevel == "use" || v.ShareLevel == "edit"
	case "admin":
		return v.HasAdminRead
	case "platform":
		return s.PlatformForUsers
	}
	return false
}

// TemplateCanEdit: shares only matter for personal templates; admin and
// platform templates are admin-edit only.
func TemplateCanEdit(t models.Template, v TemplateViewer) bool {
	if v.IsAdmin {
		return true
	}
	return t.Visibility == "personal" && (t.OwnerID == v.UserID || v.ShareLevel == "edit")
}

func TemplateCanCreate(visibility string, v TemplateViewer, s TemplateSettings) bool {
	switch visibility {
	case "admin", "platform":
		return v.IsAdmin
	case "personal":
		return v.IsAdmin || (s.Audience == "all" && (!s.CreateRequiresRole || v.HasCreate))
	}
	return false
}

// TemplateCanShare: only personal templates, only the owner or an admin.
func TemplateCanShare(t models.Template, v TemplateViewer) bool {
	return t.Visibility == "personal" && (v.IsAdmin || t.OwnerID == v.UserID)
}
