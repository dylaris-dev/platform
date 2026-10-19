package services

import (
	"regexp"
	"strconv"
	"strings"
)

// NeoForgeMinecraft maps a NeoForge build to the EXACT Minecraft version it
// targets, "" when it cannot tell. The handlers' neoForgeMajor gives only the
// line ("1.21"), and a line stored as the server's version made the Content tab
// pick mods for 1.21 on a 1.21.1 server.
//
// Below 26 the minor is Minecraft's patch: "21.1.264" is 1.21.1, "21.0.x" is
// 1.21. From 26 on the build starts with the Minecraft version itself:
// "26.1.2.5" is 26.1.2 and "26.3.0.89" is 26.3 (a .0 patch is dropped).
// Pre-releases are refused like in neoForgeMajor. database/db_phase37 does the
// same mapping in SQL for the backfill; keep the two in step.
func NeoForgeMinecraft(loader string) string {
	loader = strings.TrimSpace(loader)
	if strings.ContainsAny(loader, "-+") {
		return ""
	}
	parts := strings.Split(loader, ".")
	if len(parts) < 3 {
		return ""
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		// Nine digits, like the SQL twin, whose ::int must not overflow.
		n, err := strconv.Atoi(p)
		if err != nil || len(p) > 9 {
			return ""
		}
		nums[i] = n
	}
	if nums[0] == 0 {
		return ""
	}
	if nums[0] >= 26 {
		mc := parts[0] + "." + parts[1]
		if len(parts) >= 4 && nums[2] != 0 {
			mc += "." + parts[2]
		}
		return mc
	}
	mc := "1." + parts[0]
	if nums[1] != 0 {
		mc += "." + parts[1]
	}
	return mc
}

// ExactMinecraftVersion is the Minecraft version a setup or reinstall of this
// software installs, which is what the server records. The panel used to send
// the version LINE ("1.21") as mcVersion and nothing at all for NeoForge, and
// mod compatibility reads the recorded value as the exact version.
//
// NeoForge: derived from the loader build. Paper, vanilla, Fabric and Forge:
// installer.version is the exact Minecraft version (the providers in
// handlers/versions.go list Minecraft versions as builds), taken when mcVersion
// is empty or a line it extends. Anything else keeps mcVersion: a conflicting pair is not guessed at.
var plainDottedVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)

func ExactMinecraftVersion(software, mcVersion, version, loader string) string {
	mcVersion = strings.TrimSpace(mcVersion)
	version = strings.TrimSpace(version)
	switch software {
	case "neoforge":
		if mc := NeoForgeMinecraft(loader); mc != "" {
			return mc
		}
	case "paper", "vanilla", "fabric", "forge":
		// Plain dotted only, like phase 37's backfill: a Forge build such as
		// "1.20.1-47.3.0" extends the line "1.20" but is not a Minecraft version.
		if plainDottedVersion.MatchString(version) && (mcVersion == "" || version == mcVersion || strings.HasPrefix(version, mcVersion+".")) {
			return version
		}
	}
	return mcVersion
}

// RecordedMinecraftVersion normalises a Minecraft version read back from a
// record (an archive's manifest, a node's .dylaris.json) the way a setup writes
// it, so an old line ("1.21") or an empty NeoForge version does not land on
// the server as what mods are matched against. Unlike ExactMinecraftVersion it
// takes the build as the version only to extend a recorded line, never to fill
// an empty one: a recorded build is not known to be a Minecraft version.
func RecordedMinecraftVersion(software, mcVersion, build, loader string) string {
	if strings.TrimSpace(mcVersion) == "" {
		build = ""
	}
	return ExactMinecraftVersion(strings.ToLower(strings.TrimSpace(software)), mcVersion, build, loader)
}
