// Package validate is the single source of truth for user-input validation
// shared by core and node: identifier charsets (username, server / sub-server
// names, slugs, labels), UUIDs, safe relative paths, email, versions and
// resource bounds. One copy prevents the same field being validated
// differently - or not at all - across handlers, and guarantees identifiers are
// safe to interpolate into Redis keys and filesystem paths.
//
// The regexes here were consolidated from these former sites (kept in sync):
// core/handlers registration.go, setup.go, servers.go, servers_lifecycle.go,
// file.go, nodes.go, packs.go. The panel mirrors them in src/lib/validation.ts.
package validate

import (
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// Username is the canonical account-name rule: length 3-32, first char
	// alphanumeric, then alphanumeric / . _ - . It forbids ':' and whitespace,
	// which is what makes a username safe to interpolate into a Redis key such
	// as dylaris:beam:daily:<username>:<date>. (Was registration.go usernameRegex;
	// also replaces setup.go's stricter no-dot variant so all paths agree.)
	Username = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.\-]{2,31}$`)

	// ServerName is the unified create+rename rule: length 1-50, first char
	// alphanumeric, then alphanumeric / space / - / + / _ . (Unifies the two
	// former alphabets: servers.go's strip set and servers_lifecycle.go's rename
	// regex disagreed on space vs '_'.)
	ServerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9 \-+_]{0,49}$`)

	// SubServerName is a sub-server directory name: length 1-50, alphanumeric /
	// - / _ / + , no space. It names a directory on the node, so it must never
	// carry path metacharacters.
	SubServerName = regexp.MustCompile(`^[a-zA-Z0-9\-_+]{1,50}$`)

	// ServerUUID guards the client-supplied server identifier that becomes the %s
	// in every dylaris:server:<uuid>:* Redis key and the node's server directory
	// name. The goal is to reject injection characters (':' '/' '..' whitespace),
	// NOT to mandate a canonical UUID: the panel mints a non-canonical
	// "<ownerUUID>_<random>" id and existing servers already use it, so accept any
	// safe identifier of hex/alnum plus '-' and '_'. Length bounds keep it sane.
	ServerUUID = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,80}$`)

	// Slug is a pack slug: length 2-64, lowercase alphanumeric, inner - / _ .
	// (Was packs.go packSlugRe.)
	Slug = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{1,62}[a-z0-9])?$`)

	// Label is one node label/tag item: length 1-32, first char lower-alnum,
	// then lower alnum / - / _ .
	Label = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

	// LocationName is the name a customer gives one of their machines on the
	// infrastructure page: length 4-20, letters / digits / hyphen, and it may
	// neither start nor end with a hyphen.
	//
	// The hyphen is allowed on purpose - "home-desktop" is how people name a
	// machine - but only in the middle. A LEADING hyphen is the one that bites:
	// the name is slugged into NODE_ID and lands in a compose file, where
	// anything that shells out reads "-foo" as a flag. The panel's
	// nodeIdFromLabel already stripped those, and this makes the two agree
	// instead of one silently rewriting what the other accepted.
	//
	// 4 is a floor against "pc"; 20 keeps it readable in a list.
	LocationName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{2,18})[a-zA-Z0-9]$`)

	// Email is a light structural check. (Was registration.go emailRegex.)
	Email = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

	// McVersion is a Minecraft version like 1.21 or 1.21.4.
	McVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

	// MinecraftUsername is a Mojang account name: 3-16 chars, letters/digits/_.
	MinecraftUsername = regexp.MustCompile(`^[a-zA-Z0-9_]{3,16}$`)

	// serverNameStrip removes any character outside the ServerName alphabet.
	serverNameStrip = regexp.MustCompile(`[^a-zA-Z0-9 \-+_]`)
)

func IsUsername(s string) bool          { return Username.MatchString(s) }
func IsServerName(s string) bool        { return ServerName.MatchString(s) }
func IsSubServerName(s string) bool     { return SubServerName.MatchString(s) }
func IsServerUUID(s string) bool        { return ServerUUID.MatchString(s) }
func IsSlug(s string) bool              { return Slug.MatchString(s) }
func IsLabel(s string) bool             { return Label.MatchString(s) }
func IsLocationName(s string) bool      { return LocationName.MatchString(s) }
func IsEmail(s string) bool             { return Email.MatchString(s) }
func IsMcVersion(s string) bool         { return McVersion.MatchString(s) }
func IsMinecraftUsername(s string) bool { return MinecraftUsername.MatchString(s) }

// IsPlainFileName reports whether s names a single file with no path in it.
//
// Not a regex, because the alphabet is not the point: mod jars carry '+', '.',
// spaces and unicode, and the question is only whether the string is a NAME or
// a PATH. filepath.Base is what the node reduces it to, so agreeing with Base
// is the definition.
//
// It exists because the two ends disagreed. The node reduced a mod file name to
// its basename and refused the leftovers, which is correct and is the reason
// nothing ever escaped; Core queued the request unexamined and answered 200. So
// "../../../escape.jar" was accepted, written to disk as "escape.jar", and
// recorded - where recorded at all - under a name that is not the file. Both
// sides ask this now, so the answer Core gives is the one the node will act on.
func IsPlainFileName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, `/\`) || strings.ContainsRune(s, 0) {
		return false
	}
	return filepath.Base(s) == s
}

// SanitizeServerName coerces a raw name into the ServerName alphabet: invalid
// characters are stripped (spaces are kept, they are allowed), the result is
// trimmed to 50 characters and to a valid first character. Returns "" when
// nothing usable remains. This is the create-time best-effort coercion; the
// rename path validates with IsServerName instead.
func SanitizeServerName(raw string) string {
	s := serverNameStrip.ReplaceAllString(raw, "")
	if len(s) > 50 {
		s = s[:50]
	}
	// The first character must be alphanumeric.
	s = strings.TrimLeft(s, " -+_")
	return s
}

// IsSafeRelPath reports whether p is a safe relative path with no traversal: not
// absolute, no Windows drive / UNC prefix, and no ".." segment. Used core-side
// as defence-in-depth in front of the node's own path jail. An empty path means
// the server root and is allowed.
func IsSafeRelPath(p string) bool {
	if p == "" {
		return true
	}
	q := strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(q, "/") {
		return false // absolute
	}
	if len(q) >= 2 && q[1] == ':' {
		return false // windows drive letter
	}
	for _, seg := range strings.Split(q, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// installerTypes is the allowlist of server source/installer types.
//
// It mirrors the node's InstallServer switch, which is where these are actually
// consumed - a type accepted here that the node does not know is queued
// successfully and fails after the container already exists. "backup" is
// therefore only in this list because the node has shipped its case for it
// (release 2026.09.07.12); the node always ships first.
var installerTypes = map[string]bool{
	"paper": true, "vanilla": true, "fabric": true, "forge": true, "neoforge": true,
	"library": true, "upload": true, "upload-zip": true, "modpack": true, "pack": true,
	// Restores an uploaded Dylaris backup archive and reads the description it
	// carries. See node/installer_backup.go and services.SetupResultService.
	"backup": true,
}

// IsInstallerType reports whether t is a known server source/installer type.
func IsInstallerType(t string) bool { return installerTypes[t] }

// modrinthLoaders is the allowlist of Modrinth "loader" facet values a
// server's declared InstallerType may carry when used to drive the Content
// tab's auto-filtering (platform/panel .../servers/[id]/content/page.tsx
// LOADER_OPTIONS - kept in sync here). Deliberately broader than
// installerTypes above: that set is an install SOURCE (paper/vanilla/fabric/
// ...), this is every server platform mods/plugins can target, including
// bukkit/spigot/purpur/quilt and the proxy platforms (velocity/waterfall/
// bungeecord) which are never an install source but are valid loader tags.
var modrinthLoaders = map[string]bool{
	"paper": true, "spigot": true, "bukkit": true, "purpur": true,
	"fabric": true, "forge": true, "quilt": true, "neoforge": true,
	"velocity": true, "waterfall": true, "bungeecord": true,
}

// IsModrinthLoader reports whether t is a known Modrinth loader tag.
func IsModrinthLoader(t string) bool { return modrinthLoaders[t] }

// ResourceBounds validates a server's requested resources. ramMB is megabytes,
// cpu is cores (may be fractional), diskMB is megabytes, hostCPU is the node's
// core count (0 = unknown, so the CPU ceiling is skipped). Returns "" when the
// values are valid, otherwise a human-readable reason.
func ResourceBounds(ramMB int, cpu float64, diskMB int64, hostCPU float64) string {
	if ramMB < 256 {
		return "RAM must be at least 256 MB"
	}
	// 0 means "no limit", exactly like diskMB below, and rejecting it broke
	// server creation at the panel's own default: the wizard ships CPU LIMIT as
	// 0 and labels it "0 = no limit". The node agrees - it passes the value
	// straight to Docker as NanoCPUs, where 0 is Docker's own "uncapped" - and
	// so does the stats collector, which guards its scaling with `cpuLimit > 0`.
	// Only this check disagreed, so nothing could be created out of the box.
	if cpu < 0 {
		return "CPU limit cannot be negative"
	}
	if hostCPU > 0 && cpu > hostCPU {
		return "CPU limit exceeds the node's core count"
	}
	if diskMB < 0 {
		return "disk limit cannot be negative"
	}
	return ""
}
