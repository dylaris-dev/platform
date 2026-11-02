package modcompat

import "strings"

// DefaultTargetDirForLoader returns "mods" for fabric/forge/quilt/neoforge,
// "plugins" for paper/spigot/bukkit/purpur. Unknown loader → "mods" (safer
// default since dropping a jar in "mods" never breaks a paper server, but
// dropping a fabric mod in "plugins" leaves it silently inactive).
func DefaultTargetDirForLoader(loader string) string {
	switch strings.ToLower(loader) {
	case "paper", "spigot", "bukkit", "purpur", "pufferfish", "velocity", "waterfall", "bungeecord":
		return "plugins"
	default:
		return "mods"
	}
}
