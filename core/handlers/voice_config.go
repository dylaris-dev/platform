package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
)

// voiceConfigFiles are the config files of the two supported voice mods,
// relative to the sub-server directory. Mod builds write them under config/,
// plugin builds under plugins/. A server holds exactly one of them.
var voiceConfigFiles = []struct{ Mod, Path string }{
	{"svc", "config/voicechat/voicechat-server.properties"},
	{"svc", "plugins/voicechat/voicechat-server.properties"},
	{"plasmo", "config/plasmovoice/config.toml"},
	{"plasmo", "plugins/PlasmoVoice/config.toml"},
}

var (
	errVoiceModMissing   = errors.New("no voice chat mod config found")
	errVoiceModAmbiguous = errors.New("more than one voice chat mod config found")
)

// detectVoiceMod picks the mod from the config paths that exist. Two configs
// mean Core cannot know which one the server reads, so it writes neither.
func detectVoiceMod(found []string) (mod, path string, err error) {
	var hits []string
	for _, f := range voiceConfigFiles {
		for _, p := range found {
			if p == f.Path {
				mod, path = f.Mod, f.Path
				hits = append(hits, p)
			}
		}
	}
	switch len(hits) {
	case 0:
		return "", "", fmt.Errorf("%w (checked %d paths)", errVoiceModMissing, len(voiceConfigFiles))
	case 1:
		return mod, path, nil
	default:
		return "", "", fmt.Errorf("%w: %s", errVoiceModAmbiguous, strings.Join(hits, ", "))
	}
}

// findVoiceConfig reads every known voice config path of a sub-server and
// returns the one that exists. relPath is relative to the sub-server.
func (h *RconHandler) findVoiceConfig(nodeID int, serverUUID, subServer string) (mod, relPath, content string, err error) {
	contents := map[string]string{}
	var found []string
	for _, f := range voiceConfigFiles {
		c, ok, err := h.readNodeFileString(nodeID, serverUUID, subServer+"/"+f.Path)
		if err != nil {
			return "", "", "", fmt.Errorf("read %s: %w", f.Path, err)
		}
		if ok {
			contents[f.Path] = c
			found = append(found, f.Path)
		}
	}
	mod, relPath, err = detectVoiceMod(found)
	if err != nil {
		return "", "", "", err
	}
	return mod, relPath, contents[relPath], nil
}

func svcVoiceEnable(existing, host string, publicPort int) string {
	return mergeServerProperties(existing, map[string]string{
		"port":       strconv.Itoa(services.VoiceInternalPort),
		"voice_host": host + ":" + strconv.Itoa(publicPort),
	})
}

func svcVoiceDisable(existing string) string {
	return mergeServerProperties(existing, map[string]string{"voice_host": ""})
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

// plasmoVoiceEnable points Plasmo Voice at the in-container port and the public
// address. Line based on purpose: a TOML library would drop every comment the
// mod writes and the owner adds, and Plasmo's own null block ("#[host.public]")
// is a comment that must stay one.
func plasmoVoiceEnable(existing, host string, publicPort int) string {
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

// plasmoVoiceDisable removes the active [host.public] table: its header and
// everything up to its last key. Comments after the last key are left, they
// usually introduce the next table.
func plasmoVoiceDisable(existing string) string {
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

// syncVoiceToSubServer re-asserts a server's voice port into the voice mod
// config of the sub-server about to run, and refreshes the mapping. A server
// without a voice row is left alone: no node call, no write.
//
// Never blocks a start and never touches the RCON restart flag: every failure
// is logged and reported through the bool only.
func (h *ServerHandler) syncVoiceToSubServer(srv *models.Server, subServer string) bool {
	if subServer == "" {
		return true
	}
	if h.state == nil || h.state.Store == nil {
		return false
	}
	vp, err := h.state.Store.GetVoicePort(srv.ID)
	if err != nil {
		log.Printf("voice sync for server %d: reading the voice port failed: %v", srv.ID, err)
		return false
	}
	if vp == nil {
		return true
	}

	rh := NewRconHandler(h.state)
	mod, rel, content, err := rh.findVoiceConfig(srv.NodeID, srv.UUID, subServer)
	if err != nil {
		log.Printf("voice sync for server %d (%s): %v", srv.ID, subServer, err)
		return false
	}
	var merged string
	switch mod {
	case "plasmo":
		merged = plasmoVoiceEnable(content, vp.Host, vp.Port)
	default:
		merged = svcVoiceEnable(content, vp.Host, vp.Port)
	}
	if merged != content {
		if err := rh.writeNodeFileString(srv.NodeID, srv.UUID, subServer+"/"+rel, merged); err != nil {
			log.Printf("voice sync for server %d (%s): writing %s failed: %v", srv.ID, subServer, rel, err)
			return false
		}
	}

	// Update only the row read above, never AllocateVoicePort: that one
	// inserts, so a disable or region move landing between the read and here
	// would be undone - the row resurrected on a new port, or moved back.
	ok, err := h.state.Store.UpdateVoicePortMod(srv.ID, vp.Region, vp.Port, mod, services.VoiceInternalPort, vp.Host)
	if err != nil {
		log.Printf("voice sync for server %d: updating the voice port failed: %v", srv.ID, err)
		return false
	}
	if !ok {
		log.Printf("voice: server %d changed underneath, skipped", srv.ID)
		return false
	}
	got := *vp
	got.Mod, got.InternalPort = mod, services.VoiceInternalPort
	if h.state.Redis == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := services.PublishVoice(ctx, h.state.Redis, got); err != nil {
		log.Printf("voice sync for server %d: %v", srv.ID, err)
		return false
	}
	return true
}
