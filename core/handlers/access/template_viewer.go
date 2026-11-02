package access

import (
	"net/http"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// TemplateViewer resolves who is asking about a template, for the access rules
// in services (TemplateCanSee and siblings). t may be nil (list, create); the
// share level is only looked up for a concrete template.
func TemplateViewer(r *http.Request, az *authz.Resolver, st store.Store, t *models.Template) (services.TemplateViewer, error) {
	id := authz.IdentityFromContext(r.Context())
	v := services.TemplateViewer{UserID: id.UserID, IsAdmin: id.IsAdmin}
	if az != nil {
		if res, err := az.Resolve(id, 0); err == nil {
			v.HasAdminRead = res.HasCap("templates.admin.read")
			v.HasCreate = res.HasCap("templates.create")
		}
	}
	if t != nil && v.UserID != "" && t.OwnerID != v.UserID {
		level, err := st.TemplateShareLevel(t.ID, v.UserID)
		if err != nil {
			return v, err
		}
		v.ShareLevel = level
	}
	return v, nil
}
