package authz

// Preset is a code-defined, ready-made bundle of SERVER-scope capabilities for
// simple permissions mode (assign-only, no custom roles). A panel admin picks
// one when delegating to a friend. Every ID here MUST exist in the catalog and
// be SERVER scope (guarded by TestPresetsIntegrity); simple mode delegates
// server access, never owner-tools or panel powers.
type Preset struct {
	ID           string   `json:"id"`
	Label        string   `json:"label"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
}

var presets = []Preset{
	{
		ID:          "viewer",
		Label:       "Viewer",
		Description: "Read-only access to overview, console, stats, files, config, players and backups.",
		Capabilities: []string{
			"overview.read", "console.read", "stats.read",
			"files.read", "config.read", "players.read", "backups.read",
		},
	},
	{
		ID:          "operator",
		Label:       "Operator",
		Description: "Run the server day to day: console, power, RCON and player management.",
		Capabilities: []string{
			"overview.read", "stats.read",
			"console.read", "console.send",
			"power.start", "power.stop", "power.restart",
			"rcon.exec", "players.read", "players.manage",
		},
	},
	{
		ID:          "builder",
		Label:       "Builder",
		Description: "Everything an operator can do, plus editing files, config, mods and SFTP.",
		Capabilities: []string{
			"overview.read", "stats.read",
			"console.read", "console.send",
			"power.start", "power.stop", "power.restart",
			"rcon.exec", "players.read", "players.manage",
			"files.read", "files.write",
			"config.read", "config.write",
			"mods.read", "mods.write",
			"sftp.access",
		},
	},
	{
		ID:          "admin",
		Label:       "Server admin",
		Description: "Full control of the server short of deleting it: files, backups, members, network, schedule and settings.",
		Capabilities: []string{
			"overview.read", "console.read", "console.send",
			"power.start", "power.stop", "power.restart", "power.kill",
			"rcon.exec", "players.read", "players.manage",
			"files.read", "files.write", "files.delete", "sftp.access",
			"config.read", "config.write",
			"mods.read", "mods.write", "mods.delete",
			"backups.read", "backups.create", "backups.delete", "backups.restore",
			"network.read", "network.write",
			"domains.read", "domains.write",
			"tabs.read", "tabs.write",
			"schedule.read", "schedule.write", "schedule.delete",
			"stats.read", "spark.use",
			"members.read", "members.write", "members.delete",
			"server.settings.write",
		},
	},
}

// Presets returns a copy of the code-defined preset bundles. The Capabilities
// slices are shared (read-only by convention; callers only render/encode them).
func Presets() []Preset {
	out := make([]Preset, len(presets))
	copy(out, presets)
	return out
}
