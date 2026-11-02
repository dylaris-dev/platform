// Package voicecfg rewrites the config files of the two supported voice chat
// mods (Simple Voice Chat, Plasmo Voice) and server.properties. Pure text
// transforms: the node I/O around them stays in the handlers.
package voicecfg

import (
	"sort"
	"strconv"
	"strings"

	"dylaris-core/services"
)

// MergeServerProperties returns the server.properties content with each key in
// kv set to its value, preserving every other line, comment, and the existing
// ordering. A key already present has only its value replaced; a key not
// present is appended (appended keys are emitted in sorted order for a
// deterministic result). The Minecraft server.properties format is a flat
// key=value file (one pair per line, '#' or '!' introduces a comment), which
// is what this parses - no line continuations, no escaping.
//
// This is how RCON config reaches the actual server: Core owns the DB flags,
// but MC only opens the RCON port if enable-rcon/rcon.port/rcon.password live
// in server.properties, so on enable we merge them in and the file (durable in
// the server directory) keeps them across restarts.
func MergeServerProperties(existing string, kv map[string]string) string {
	remaining := make(map[string]string, len(kv))
	for k, v := range kv {
		remaining[k] = v
	}

	// Normalize CRLF so we split cleanly; MC writes LF.
	lines := strings.Split(strings.ReplaceAll(existing, "\r\n", "\n"), "\n")

	// A trailing newline yields a final empty element; drop it so we don't
	// grow a blank line on every write, then re-add exactly one at the end.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	out := make([]string, 0, len(lines)+len(kv))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "!") {
			out = append(out, line)
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			out = append(out, line)
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if newVal, ok := remaining[key]; ok {
			out = append(out, key+"="+newVal)
			delete(remaining, key)
			continue
		}
		out = append(out, line)
	}

	appendKeys := make([]string, 0, len(remaining))
	for k := range remaining {
		appendKeys = append(appendKeys, k)
	}
	sort.Strings(appendKeys)
	for _, k := range appendKeys {
		out = append(out, k+"="+remaining[k])
	}

	return strings.Join(out, "\n") + "\n"
}

// SvcEnable points Simple Voice Chat at the in-container port and the
// public address.
func SvcEnable(existing, host string, publicPort int) string {
	return MergeServerProperties(existing, map[string]string{
		"port":       strconv.Itoa(services.VoiceInternalPort),
		"voice_host": host + ":" + strconv.Itoa(publicPort),
	})
}

// SvcDisable clears the public address again.
func SvcDisable(existing string) string {
	return MergeServerProperties(existing, map[string]string{"voice_host": ""})
}

// tomlHeader returns the table name of a TOML header line ("[host.public]" ->
// "host.public"), or ok=false for anything else, comments included.
func tomlHeader(line string) (name string, ok bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") {
		return "", false
	}
	end := strings.IndexByte(t, ']')
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(strings.Trim(t[:end], "[")), true
}

// tomlKey returns the key of a "key = value" line, or "" for anything else.
func tomlKey(line string) string {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "[") {
		return ""
	}
	eq := strings.IndexByte(t, '=')
	if eq < 0 {
		return ""
	}
	return strings.TrimSpace(t[:eq])
}

func splitConfigLines(s string) []string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func joinConfigLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}

// tomlSection returns the header index of table name and the index of the next
// header after it (len(lines) when none), or -1, -1.
func tomlSection(lines []string, name string) (start, end int) {
	start = -1
	for i, l := range lines {
		n, ok := tomlHeader(l)
		if !ok {
			continue
		}
		if start >= 0 {
			return start, i
		}
		if n == name {
			start = i
		}
	}
	if start < 0 {
		return -1, -1
	}
	return start, len(lines)
}

func insertLines(lines []string, at int, add ...string) []string {
	out := make([]string, 0, len(lines)+len(add))
	out = append(out, lines[:at]...)
	out = append(out, add...)
	return append(out, lines[at:]...)
}

// setTomlKeys sets each key inside the section [start, end) in place, and adds
// the missing ones right after the section's last key (or its header).
func setTomlKeys(lines []string, start, end int, keys []string, vals map[string]string) []string {
	last := start
	var missing []string
	for _, k := range keys {
		hit := false
		for i := start + 1; i < end; i++ {
			if tomlKey(lines[i]) == k {
				lines[i] = k + " = " + vals[k]
				hit = true
			}
		}
		if !hit {
			missing = append(missing, k+" = "+vals[k])
		}
	}
	for i := start + 1; i < end; i++ {
		if tomlKey(lines[i]) != "" {
			last = i
		}
	}
	return insertLines(lines, last+1, missing...)
}

// endBeforeBlanks backs up from end over the blank lines that separate a
// section from the next header, so an insert lands inside the block.
func endBeforeBlanks(lines []string, start, end int) int {
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return end
}

// PlasmoEnable points Plasmo Voice at the in-container port and the public
// address. Line based on purpose: a TOML library would drop every comment the
// mod writes and the owner adds, and Plasmo's own null block ("#[host.public]")
// is a comment that must stay one.
func PlasmoEnable(existing, host string, publicPort int) string {
	lines := splitConfigLines(existing)
	internal := strconv.Itoa(services.VoiceInternalPort)

	hs, he := tomlSection(lines, "host")
	if hs < 0 {
		lines = append(lines, "", "[host]", "port = "+internal)
	} else {
		lines = setTomlKeys(lines, hs, he, []string{"port"}, map[string]string{"port": internal})
	}

	pub := map[string]string{"ip": strconv.Quote(host), "port": strconv.Itoa(publicPort)}
	if ps, pe := tomlSection(lines, "host.public"); ps >= 0 {
		lines = setTomlKeys(lines, ps, pe, []string{"ip", "port"}, pub)
		return joinConfigLines(lines)
	}

	// After the [host] block and any [host.*] subtables, before the next
	// unrelated header.
	hs, _ = tomlSection(lines, "host")
	at := len(lines)
	for i := hs + 1; i < len(lines); i++ {
		if n, ok := tomlHeader(lines[i]); ok && !strings.HasPrefix(n, "host.") {
			at = i
			break
		}
	}
	at = endBeforeBlanks(lines, hs, at)
	lines = insertLines(lines, at, "", "[host.public]", "ip = "+pub["ip"], "port = "+pub["port"])
	return joinConfigLines(lines)
}

// PlasmoDisable removes the active [host.public] table: its header and
// everything up to its last key. Comments after the last key are left, they
// usually introduce the next table.
func PlasmoDisable(existing string) string {
	lines := splitConfigLines(existing)
	ps, pe := tomlSection(lines, "host.public")
	if ps < 0 {
		return joinConfigLines(lines)
	}
	last := ps
	for i := ps + 1; i < pe; i++ {
		if tomlKey(lines[i]) != "" {
			last = i
		}
	}
	from, to := ps, last+1
	// Take the blank line that separated the table along, so enable then
	// disable gives the file back unchanged.
	if from > 0 && strings.TrimSpace(lines[from-1]) == "" && (to == len(lines) || strings.TrimSpace(lines[to]) == "") {
		from--
	}
	return joinConfigLines(append(lines[:from:from], lines[to:]...))
}
