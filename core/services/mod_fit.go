package services

import (
	"strconv"
	"strings"
)

// modLoadersFor maps a server's installer type to the Modrinth loader tags it
// can load. Quilt loads Fabric mods; NeoForge does NOT load Forge mods (they
// split at 1.20.2 and a Forge jar on NeoForge fails at boot). A Paper server
// runs Spigot and Bukkit plugins, not the other way round.
var modLoadersFor = map[string][]string{
	"forge":      {"forge"},
	"neoforge":   {"neoforge"},
	"fabric":     {"fabric"},
	"quilt":      {"quilt", "fabric"},
	"bukkit":     {"bukkit"},
	"spigot":     {"spigot", "bukkit"},
	"paper":      {"paper", "spigot", "bukkit"},
	"purpur":     {"purpur", "paper", "spigot", "bukkit"},
	"velocity":   {"velocity"},
	"waterfall":  {"waterfall", "bungeecord"},
	"bungeecord": {"bungeecord"},
}

// A proxy records its own release ("3.5") as the Minecraft version, which no
// plugin lists as a game version, so its plugins are matched by loader alone.
func isProxyLoader(loader string) bool {
	switch loader {
	case "velocity", "waterfall", "bungeecord":
		return true
	}
	return false
}

// Bukkit-family plugins are built against the server API, which holds across
// the patch releases of one Minecraft line: a plugin tagged 1.21.3 runs on
// Paper 1.21.4, and plugin authors rarely tag every patch. Mods hook the game
// itself, where a patch release breaks them, so mods stay on the exact version.
func isPluginLoader(loader string) bool {
	switch loader {
	case "bukkit", "spigot", "paper", "purpur":
		return true
	}
	return false
}

// minecraftLine is the first two numbers of a version: 1.21.4 -> 1.21, and in
// the 26.x scheme 26.1.2 -> 26.1. A version with no second part is its own line.
func minecraftLine(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// ModFitKnown reports whether the server says enough about itself to check a
// mod against it. An imported server (installer "upload", no MC version) does
// not, and installs onto it are not checked.
func ModFitKnown(serverLoader, serverMC string) bool {
	loader := strings.ToLower(strings.TrimSpace(serverLoader))
	if _, ok := modLoadersFor[loader]; !ok {
		return false
	}
	return isProxyLoader(loader) || strings.TrimSpace(serverMC) != ""
}

// ModVersionFits decides whether a Modrinth version (its loaders and
// game_versions) runs on a server with this installer type and exact Minecraft
// version: the exact version for mods, the same line for plugins. reason is an English sentence for the user when it does not.
// Callers check ModFitKnown first; an unknown loader or MC version is refused.
func ModVersionFits(serverLoader, serverMC string, versionLoaders, versionGameVersions []string) (ok bool, reason string) {
	loader := strings.ToLower(strings.TrimSpace(serverLoader))
	mc := strings.TrimSpace(serverMC)
	accepted, known := modLoadersFor[loader]
	if !known {
		return false, "The server's loader is unknown. Declare it first."
	}
	loaderOK := false
	for _, vl := range versionLoaders {
		for _, a := range accepted {
			if strings.EqualFold(strings.TrimSpace(vl), a) {
				loaderOK = true
			}
		}
	}
	if !loaderOK {
		return false, "This build is for " + listForReason(versionLoaders) + ", and this server runs " + loader + ". Pick a " + loader + " build."
	}
	if isProxyLoader(loader) {
		return true, ""
	}
	if mc == "" {
		return false, "The server's Minecraft version is unknown. Declare it first."
	}
	plugin := isPluginLoader(loader)
	for _, gv := range versionGameVersions {
		gv = strings.TrimSpace(gv)
		if gv == mc || plugin && minecraftLine(gv) == minecraftLine(mc) {
			return true, ""
		}
	}
	want := mc
	if plugin {
		want = minecraftLine(mc) + ".x"
	}
	return false, "This build is for Minecraft " + listForReason(versionGameVersions) + ", and this server runs " + mc + ". Pick a build for " + want + "."
}

// listForReason keeps the message short; Modrinth lists game versions oldest
// first, so the last ones are the interesting ones.
func listForReason(items []string) string {
	if len(items) == 0 {
		return "nothing"
	}
	if len(items) > 3 {
		return strings.Join(items[len(items)-3:], ", ") + " (and " + strconv.Itoa(len(items)-3) + " older)"
	}
	return strings.Join(items, ", ")
}
