package services

// Who may do what on a Modrinth project, as Modrinth itself decides it. The
// panel's access to a pack bound to a project follows this (owner decision
// 2026-10-28): there is no second permission system in the panel.
//
// Resolution mirrors labrinth's ProjectPermissions::get_permissions_by_role:
// an accepted member of the project's own team gets that member's permissions;
// otherwise an accepted member of the owning organization gets the
// organization's default project permissions (their "permissions" field);
// otherwise nothing. Team owners get every permission.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// Modrinth's ProjectPermissions bits (apps/labrinth/src/models/v3/teams.rs).
const (
	ModrinthPermUploadVersion int64 = 1 << 0
	ModrinthPermDeleteVersion int64 = 1 << 1
	ModrinthPermEditDetails   int64 = 1 << 2
	ModrinthPermEditBody      int64 = 1 << 3
	ModrinthPermDeleteProject int64 = 1 << 7
	ModrinthPermAll           int64 = (1 << 10) - 1
)

// ModrinthTeamMember is one row of a project's or organization's team.
type ModrinthTeamMember struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Role        string `json:"role"`
	Permissions *int64 `json:"permissions"`
	Accepted    bool   `json:"accepted"`
	IsOwner     bool   `json:"is_owner"`
}

// ErrModrinthScopeMissing: the token works but Modrinth answered as if it were
// anonymous. labrinth drops a token that lacks the read scope of a route
// (PROJECT_READ, ORGANIZATION_READ) to "no user" instead of refusing it, and
// then shows the team without anyone's permissions.
var ErrModrinthScopeMissing = errors.New("the Modrinth token lacks the PROJECT_READ or ORGANIZATION_READ scope")

// ModrinthTokenRejected reports whether err is Modrinth refusing the token
// itself (revoked, expired or missing scopes): the user must renew it.
func ModrinthTokenRejected(err error) bool {
	var httpErr *ModrinthHTTPError
	return errors.Is(err, ErrModrinthScopeMissing) || (errors.As(err, &httpErr) && httpErr.Status == http.StatusUnauthorized)
}

// ProjectTeam lists the project's own team. Permissions are only visible to a
// caller who is a member, which is the only case this is used for.
func (c *ModrinthClient) ProjectTeam(ctx context.Context, projectID string) ([]ModrinthTeamMember, error) {
	var out []ModrinthTeamMember
	err := c.doJSON(ctx, http.MethodGet, modrinthAPIv3+"/project/"+url.PathEscape(projectID)+"/members", nil, &out)
	return out, err
}

// ProjectOrganization is the id of the organization owning the project, or "".
func (c *ModrinthClient) ProjectOrganization(ctx context.Context, projectID string) (string, error) {
	var p struct {
		Organization *string `json:"organization"`
	}
	if err := c.doJSON(ctx, http.MethodGet, modrinthAPIv3+"/project/"+url.PathEscape(projectID), nil, &p); err != nil {
		return "", err
	}
	if p.Organization == nil {
		return "", nil
	}
	return *p.Organization, nil
}

// OrganizationTeam lists an organization's members; for an organization
// member, "permissions" is the default project permission set.
func (c *ModrinthClient) OrganizationTeam(ctx context.Context, orgID string) ([]ModrinthTeamMember, error) {
	var out []ModrinthTeamMember
	err := c.doJSON(ctx, http.MethodGet, modrinthAPIv3+"/organization/"+url.PathEscape(orgID)+"/members", nil, &out)
	return out, err
}

// UserOrganizationProjects lists the modpack projects of every organization
// the user belongs to (GET /v2/user/{id}/projects leaves those out).
func (c *ModrinthClient) UserOrganizationProjects(ctx context.Context, userID string) ([]ModrinthProjectSummary, error) {
	var orgs []struct {
		ID string `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodGet, modrinthAPIv3+"/user/"+url.PathEscape(userID)+"/organizations", nil, &orgs); err != nil {
		return nil, err
	}
	out := []ModrinthProjectSummary{}
	for _, o := range orgs {
		var projects []struct {
			ID           string   `json:"id"`
			Slug         string   `json:"slug"`
			Name         string   `json:"name"`
			Summary      string   `json:"summary"`
			Status       string   `json:"status"`
			IconURL      *string  `json:"icon_url"`
			Versions     []string `json:"versions"`
			ProjectTypes []string `json:"project_types"`
		}
		if err := c.doJSON(ctx, http.MethodGet, modrinthAPIv3+"/organization/"+url.PathEscape(o.ID)+"/projects", nil, &projects); err != nil {
			return nil, err
		}
		for _, p := range projects {
			for _, t := range p.ProjectTypes {
				if t == "modpack" {
					out = append(out, ModrinthProjectSummary{ID: p.ID, Slug: p.Slug, Title: p.Name, Description: p.Summary, ProjectType: "modpack", Status: p.Status, IconURL: p.IconURL, Versions: p.Versions})
					break
				}
			}
		}
	}
	return out, nil
}

// ProjectPermissionsFor resolves what the user may do on the project, the way
// Modrinth does. member=false means the user is in neither team.
func (c *ModrinthClient) ProjectPermissionsFor(ctx context.Context, projectID, modrinthUserID string) (perms int64, member bool, err error) {
	team, err := c.ProjectTeam(ctx, projectID)
	if err != nil {
		return 0, false, err
	}
	if p, ok, err := memberPermissions(team, modrinthUserID); ok || err != nil {
		return p, ok, err
	}
	orgID, err := c.ProjectOrganization(ctx, projectID)
	if err != nil || orgID == "" {
		return 0, false, err
	}
	orgTeam, err := c.OrganizationTeam(ctx, orgID)
	if err != nil {
		return 0, false, err
	}
	return memberPermissions(orgTeam, modrinthUserID)
}

// memberPermissions finds the user in a team. An accepted member always sees
// their own permissions, so a member listed without them means Modrinth read
// the token as anonymous: ErrModrinthScopeMissing.
func memberPermissions(team []ModrinthTeamMember, userID string) (int64, bool, error) {
	for _, m := range team {
		if m.User.ID != userID || !m.Accepted {
			continue
		}
		if m.IsOwner {
			return ModrinthPermAll, true, nil
		}
		if m.Permissions == nil {
			return 0, false, ErrModrinthScopeMissing
		}
		return *m.Permissions, true, nil
	}
	return 0, false, nil
}
