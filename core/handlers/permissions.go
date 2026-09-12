package handlers

import (
	"strings"

	"dylaris-core/models"
)

// EffectivePermissions is the materialized set of capability flags for one user.
// Admins always get everything; regular users get what their role + per-user
// flags allow. The Role field reflects the canonical role string ("user" /
// "support" / "admin") so downstream code doesn't have to recompute it.
//
// Compute once per request and pass around instead of re-reading the user
// row in every gate — both cheaper and avoids stale-state races.
type EffectivePermissions struct {
	Role                string
	IsAdmin             bool
	IsSupport           bool
	CanAccessAllRegions bool
	AllowedRegions      []string
	CanDeleteServers    bool
	CanChangeResources  bool
}

// ComputeEffectivePermissions materializes the permissions for a user.
// Admin-shortcut: an admin is_admin or role=='admin' grants everything,
// regardless of per-user flag values. This matches the legacy contract
// where is_admin was the only gate.
func ComputeEffectivePermissions(user *models.User, allowedRegions []string) EffectivePermissions {
	if user == nil {
		return EffectivePermissions{Role: "user"}
	}
	role := user.Role
	if role == "" {
		role = "user"
	}
	if user.IsAdmin || role == "admin" {
		return EffectivePermissions{
			Role:                "admin",
			IsAdmin:             true,
			CanAccessAllRegions: true,
			CanDeleteServers:    true,
			CanChangeResources:  true,
		}
	}
	// Deleting a server is ADMIN-ONLY, and that is a role property rather than
	// a per-user switch: the stored can_delete_servers flag is deliberately not
	// read here.
	//
	// A hoster's customer cancels, they do not delete - and a paid server
	// removed by accident is a support case nobody wins, because the data is
	// gone with it. Support does not get it either: looking at a tenant's
	// server is the job, removing it is not.
	//
	// The flag is still stored and still written by the permissions endpoint,
	// which forces it false for non-admins (see SetUserPermissionsHandler), so
	// there is no row that claims a right nobody has.
	return EffectivePermissions{
		Role:                role,
		IsAdmin:             false,
		IsSupport:           role == "support",
		CanAccessAllRegions: user.AllRegionsAccess,
		AllowedRegions:      allowedRegions,
		CanDeleteServers:    false,
		// Scoped by the CAPABILITY on the route, not by this flag: every call
		// site pairs it with RequireCap("server.settings.write"), which the
		// resolver evaluates against the one server in the request. So this
		// grants the ability at all, and ownership or an explicit grant decides
		// where - a user reaches their own servers, support reaches what it was
		// given, an admin reaches everything.
		CanChangeResources: user.CanChangeResources,
	}
}

// CanAccessRegion is the single gate used by region-aware list/show endpoints.
// Admins and all-regions users pass everything; explicit-region users get a
// set-membership check.
func (p EffectivePermissions) CanAccessRegion(regionID string) bool {
	if p.CanAccessAllRegions {
		return true
	}
	regionID = strings.ToLower(strings.TrimSpace(regionID))
	if regionID == "" {
		// Defensive: a server with no region (shouldn't happen post-migration
		// since the column defaults to 'default') is treated as default.
		regionID = "default"
	}
	// Case- and space-insensitive, like services.equalRegion and PickBeamRelay.
	// This was the one region comparison in the platform that demanded an exact
	// match, and it is the one where a mismatch is silent: the other two fall
	// back (any relay, no move), this one HIDES a server from the staff member
	// meant to see it. Region ids are canonically lowercase, so folding cannot
	// merge two distinct regions - CreateRegion lowercases and the id regex
	// forbids the rest. It only forgives rows written before that was enforced.
	for _, r := range p.AllowedRegions {
		if strings.EqualFold(strings.TrimSpace(r), regionID) {
			return true
		}
	}
	return false
}

// FilterServersByRegion drops servers whose region the user can't access -
// EXCEPT the ones they own, which are never hidden from them.
//
// Regions are a STAFF visibility tool: they answer "support may look at EU
// only". Applied to an owner they answer a question nobody asked and get it
// wrong. A customer's region set is empty unless somebody filled it in, and the
// default for a self-registered account is no regions at all, so this dropped
// every server the account had - including the one it had just created. The
// server ran, the admin could see it, and its owner could not. That is what BYON
// testing hit.
//
// Nor does it hide a server the viewer was INVITED to, for the same reason one
// step further out. An invitation is the owner saying "this person may use my
// server"; a region set is the operator saying how far a member of staff may
// look. Applying the second to the first dropped the invited server from the
// list while RequireCap kept letting the invitee open it by URL - the list and
// the authorization disagreeing, which is the defect, not the region set.
//
// viewerID is the caller. Empty means "no identity", which keeps the old
// behaviour for any path that cannot name one rather than silently widening it.
func FilterServersByRegion(servers []models.Server, p EffectivePermissions, viewerID string) []models.Server {
	if p.CanAccessAllRegions {
		return servers
	}
	out := make([]models.Server, 0, len(servers))
	for _, s := range servers {
		mine := viewerID != "" && (s.OwnerID == viewerID || s.Role == "invited" || s.Role == "inherited")
		if mine || p.CanAccessRegion(s.Region) {
			out = append(out, s)
		}
	}
	return out
}

// LoadEffectivePermissions is a convenience for handlers: fetches the user
// + their region IDs, then computes the permissions. Returns zero-value
// permissions (role="user", everything denied) if the lookup fails.
func LoadEffectivePermissions(state *AppState, userID string) EffectivePermissions {
	if state == nil || state.Store == nil || userID == "" {
		return EffectivePermissions{Role: "user"}
	}
	user, err := state.Store.GetUserByID(userID)
	if err != nil || user == nil {
		return EffectivePermissions{Role: "user"}
	}
	regions, _ := state.Store.GetUserRegionIDs(userID)
	return ComputeEffectivePermissions(user, regions)
}
