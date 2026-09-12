package handlers

import (
	"dylaris-core/authz"
	"dylaris-core/models"
)

// The panel gates a server's tabs on the 9-bool TabPermissions blob that comes
// back with each server row, and that blob was read straight off the legacy
// invite. A grant made through the Access page writes cap_overrides and a
// server_role_id, never the legacy blob - so a member granted backups.read got
// the data from the API and a locked Backups tab in the panel. Every tab looked
// locked, in fact: the blob was all-false for anyone whose access came from the
// cap model rather than an old-style invite.
//
// Deriving the blob from the resolved caps makes the navigation say what the
// API already enforces. The bits are OR-ed onto whatever the invite carries, so
// a legacy invite keeps exactly the tabs it had.
//
// tabPermissionCaps maps each tab bit to the capability that represents it -
// the read cap where the tab has one, which is what "can open this tab" means.
// Inherit is deliberately absent: it is an invite column, not a capability.
var tabPermissionCaps = []struct {
	cap string
	set func(*models.TabPermissions)
}{
	{"console.read", func(p *models.TabPermissions) { p.Console = true }},
	{"files.read", func(p *models.TabPermissions) { p.Files = true }},
	{"config.read", func(p *models.TabPermissions) { p.Config = true }},
	{"power.start", func(p *models.TabPermissions) { p.Power = true }},
	// The Players tab used to open on power.start - "may start the server"
	// decided "may see the player tools", because players.read gated nothing at
	// all. It has its own routes now, so it gets its own bit.
	{"players.read", func(p *models.TabPermissions) { p.Players = true }},
	{"network.read", func(p *models.TabPermissions) { p.Network = true }},
	{"overview.read", func(p *models.TabPermissions) { p.Overview = true }},
	{"members.read", func(p *models.TabPermissions) { p.Members = true }},
	{"backups.read", func(p *models.TabPermissions) { p.Backups = true }},
	{"server.settings.write", func(p *models.TabPermissions) { p.Setup = true }},
}

// mergeResolvedTabPermissions turns a resolved capability set into tab bits and
// ORs them onto base. base may be nil (a member with no legacy invite at all).
func mergeResolvedTabPermissions(base *models.TabPermissions, hasCap func(string) bool) models.TabPermissions {
	var out models.TabPermissions
	if base != nil {
		out = *base
	}
	if hasCap == nil {
		return out
	}
	for _, m := range tabPermissionCaps {
		if hasCap(m.cap) {
			m.set(&out)
		}
	}
	return out
}

// applyResolvedTabPermissions fills in the tab bits for every server the caller
// reaches through an invite, a server-role or a grant, and DROPS the ones the
// resolver gives no server capability on at all. Owner and admin rows are left
// alone: the panel short-circuits both on role, and resolving a cap set per
// owned server would be work nobody reads.
//
// Why the list is decided here and not in SQL: ListServersForUser selects every
// server a grant ROW points at, and a row is not a permission. An account-wide
// grant may carry only OWNER caps (modpacks, backup storage) and still matches
// every server its owner has; a direct invite may have had every cap revoked.
// Which scope a capability belongs to lives in the Go catalog, so the query can
// only promise not to MISS a server, and the resolver removes what it will not
// open. Before, such a member got every one of those servers in their list -
// node address, ports, start command - and a 403 on each.
//
// A resolver error drops the row. This is a membership decision, and the old
// "keep the legacy blob rather than answer with less" is the wrong direction for
// that: the list query has just succeeded, so an error here is not an outage
// that would empty everybody's list.
func applyResolvedTabPermissions(state *AppState, servers []models.Server, userID, username string) []models.Server {
	if state == nil || state.Authz == nil || userID == "" {
		return servers
	}
	identity := authz.Identity{UserID: userID, Username: username}
	out := servers[:0]
	for _, s := range servers {
		if s.Role != "invited" && s.Role != "inherited" {
			out = append(out, s)
			continue
		}
		res, err := state.Authz.Resolve(identity, s.ID)
		if err != nil || !res.HasAnyServerCap() {
			continue
		}
		merged := mergeResolvedTabPermissions(s.Permissions, res.HasCap)
		s.Permissions = &merged
		out = append(out, s)
	}
	return out
}
