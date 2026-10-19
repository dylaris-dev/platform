package database

import (
	"database/sql"
	"fmt"
)

// neoForgeMinecraftSQL maps a NeoForge loader build (the column l) to the
// exact Minecraft version it targets, empty when it cannot tell. The SQL twin of
// services.NeoForgeMinecraft; keep the two in step. "21.1.264" -> 1.21.1,
// "21.0.5" -> 1.21, "26.1.2.5" -> 26.1.2, "26.3.0.89" -> 26.3.
//
// The regex admits only plain dotted numbers of at most nine digits, so every
// ::int below is safe: a pre-release ("-beta", "+snapshot") or anything else
// maps to empty and is left alone, never an error at boot.
const neoForgeMinecraftSQL = `CASE
	WHEN l !~ '^[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}(\.[0-9]{1,9})*$' THEN ''
	WHEN split_part(l, '.', 1)::int = 0 THEN ''
	WHEN split_part(l, '.', 1)::int >= 26 THEN
		split_part(l, '.', 1) || '.' || split_part(l, '.', 2) ||
		CASE WHEN split_part(l, '.', 4) <> '' AND split_part(l, '.', 3)::int <> 0
			THEN '.' || split_part(l, '.', 3) ELSE '' END
	ELSE
		'1.' || split_part(l, '.', 1) ||
		CASE WHEN split_part(l, '.', 2)::int <> 0 THEN '.' || split_part(l, '.', 2) ELSE '' END
END`

// applyExactMcVersionBackfill records the EXACT Minecraft version on servers
// set up before Core derived it. The panel sent the version line ("1.21") as
// mcVersion and nothing at all for NeoForge, so mod installs were matched
// against 1.21 on a 1.21.1 server, or against nothing.
//
// NeoForge: the version comes from the recorded loader, on sub_server_installs
// directly and on servers through the active sub-server's install row. Written
// where empty, or where the stored value is the line the mapped one extends.
// An empty build becomes the loader, which is what setup records for NeoForge
// now; the edit form compares builds and read an empty build against the
// loader as a version change on every save.
//
// Paper, vanilla, Fabric, Forge: the build IS the exact Minecraft version, so
// it replaces the version only where it extends the stored line ("1.21" with
// "1.21.1"). The build must be plain dotted numbers: a version update records a
// loader build there ("0.16.9", "1.20.1-47.3.0"), which no rule here may read
// as a Minecraft version. Every other row is left as it is.
//
// Idempotent: a second run finds nothing that still matches.
func applyExactMcVersionBackfill(db *sql.DB) error {
	for _, q := range []string{
		`UPDATE sub_server_installs i SET mc_version = m.mc
			FROM (SELECT server_id, sub_server_name, ` + neoForgeMinecraftSQL + ` AS mc
				FROM (SELECT server_id, sub_server_name, btrim(loader) AS l FROM sub_server_installs
					WHERE installer_type = 'neoforge') x) m
			WHERE i.server_id = m.server_id AND i.sub_server_name = m.sub_server_name
				AND m.mc <> '' AND i.mc_version <> m.mc
				AND (i.mc_version = '' OR left(m.mc, length(i.mc_version) + 1) = i.mc_version || '.')`,
		`UPDATE servers s SET minecraft_version = m.mc
			FROM (SELECT server_id, sub_server_name, ` + neoForgeMinecraftSQL + ` AS mc
				FROM (SELECT server_id, sub_server_name, btrim(loader) AS l FROM sub_server_installs
					WHERE installer_type = 'neoforge') x) m
			WHERE s.id = m.server_id AND s.active_sub_server = m.sub_server_name
				AND s.installer_type = 'neoforge'
				AND m.mc <> '' AND COALESCE(s.minecraft_version, '') <> m.mc
				AND (COALESCE(s.minecraft_version, '') = ''
					OR left(m.mc, length(s.minecraft_version) + 1) = s.minecraft_version || '.')`,
		`UPDATE sub_server_installs SET build_version = btrim(loader)
			WHERE installer_type = 'neoforge' AND build_version = '' AND btrim(loader) <> ''`,
		`UPDATE servers s SET build_number = btrim(i.loader)
			FROM sub_server_installs i
			WHERE i.server_id = s.id AND i.sub_server_name = s.active_sub_server
				AND s.installer_type = 'neoforge' AND i.installer_type = 'neoforge'
				AND COALESCE(s.build_number, '') = '' AND btrim(i.loader) <> ''`,
		`UPDATE sub_server_installs SET mc_version = build_version
			WHERE installer_type IN ('paper', 'vanilla', 'fabric', 'forge')
				AND mc_version <> ''
				AND build_version ~ '^[0-9]+(\.[0-9]+)+$'
				AND left(build_version, length(mc_version) + 1) = mc_version || '.'`,
		`UPDATE servers SET minecraft_version = build_number
			WHERE installer_type IN ('paper', 'vanilla', 'fabric', 'forge')
				AND COALESCE(minecraft_version, '') <> ''
				AND build_number ~ '^[0-9]+(\.[0-9]+)+$'
				AND left(build_number, length(minecraft_version) + 1) = minecraft_version || '.'`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("exact mc version backfill: %w", err)
		}
	}
	return nil
}
