package store

import (
	"sort"

	"dylaris-core/models"
)

// MapLegacyInviteCaps maps the legacy 9-bool TabPermissions blob to the new
// granular SERVER cap set (phase 3 migration). Field order is fixed so the
// output is deterministic. Inherit is NOT a cap (it stays the invite.inherit
// column). There was no legacy backups bit, so a migrated friend gets NO
// backups.* caps - this preserves the old behavior where backups were
// owner/admin only. Every id here is a real ScopeServer cap in
// authz/catalog.go (enforced by db_phase21_invite_migration_test.go in the
// database package, which already imports both store and authz - this
// package cannot import authz itself, since authz imports store).
//
// Exported so both the one-time boot migration (database package) and the
// live member write path (CreateInvite/UpdateInvitePermissions below) derive
// cap_overrides from the exact same mapping, keeping a migrated row and an
// inline-written row indistinguishable.
func MapLegacyInviteCaps(p models.TabPermissions) []string {
	caps := []string{}
	if p.Console {
		caps = append(caps, "console.read", "console.send")
	}
	if p.Files {
		caps = append(caps, "files.read", "files.write", "files.delete")
	}
	if p.Config {
		caps = append(caps, "config.read", "config.write", "mods.read", "mods.write", "mods.delete")
	}
	if p.Power {
		caps = append(caps, "power.start", "power.stop", "power.restart", "power.kill", "rcon.exec", "players.read", "players.manage")
	}
	if p.Network {
		caps = append(caps, "network.read", "network.write")
	}
	if p.Overview {
		caps = append(caps, "overview.read")
	}
	if p.Members {
		caps = append(caps, "members.read", "members.write", "members.delete")
	}
	if p.Setup {
		caps = append(caps, "server.settings.write")
	}
	return caps
}

// TabPermissionsFromCaps summarises a member's effective SERVER capabilities in
// the legacy nine-bool shape: one bool per tab, true when the member can open
// it at all. Inherit is not a capability and is carried by its own column, so
// it is always false here and the caller sets it.
//
// This exists because the member roster answers "who may do what on my server"
// and it was answering it from the legacy blob alone. A member added through
// POST /api/grants - the route the panel's Access page actually uses - has an
// empty blob and real capabilities, so the roster printed every flag false for
// somebody holding full server admin. Measured on production.
//
// It is a summary, not an inverse: the read cap is what decides, because that
// is what opens the tab. Note that legacy "power" also conferred players.read,
// so a migrated invite reports Players true - which is the tab that member has
// had all along.
func TabPermissionsFromCaps(caps []string) models.TabPermissions {
	has := make(map[string]bool, len(caps))
	for _, c := range caps {
		has[c] = true
	}
	return models.TabPermissions{
		Console:  has["console.read"],
		Files:    has["files.read"],
		Config:   has["config.read"],
		Setup:    has["server.settings.write"],
		Overview: has["overview.read"],
		Power:    has["power.start"] || has["power.stop"] || has["power.restart"] || has["power.kill"],
		Players:  has["players.read"],
		Members:  has["members.read"],
		Network:  has["network.read"],
		Backups:  has["backups.read"],
	}
}

// EffectiveGrantCaps folds a role's capabilities and a grant's overrides into
// the set the member actually holds, in the same order applyGrant does it:
// role, then grant, then deny. Sorted so a roster row is stable between reads.
func EffectiveGrantCaps(roleCaps []string, ov CapOverrides) []string {
	set := map[string]bool{}
	for _, c := range roleCaps {
		set[c] = true
	}
	for _, c := range ov.Grant {
		set[c] = true
	}
	for _, c := range ov.Deny {
		delete(set, c)
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
