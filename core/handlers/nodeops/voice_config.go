package nodeops

import (
	nodegrpc "dylaris-core/grpc"
	"errors"
	"fmt"
	"strings"
)

// voiceConfigFiles are the config files of the two supported voice mods,
// relative to the sub-server directory. Mod builds write them under config/,
// plugin builds under plugins/. A server holds exactly one of them.
var voiceConfigFiles = []struct{ Mod, Path string }{
	{"svc", "config/voicechat/voicechat-server.properties"},
	{"svc", "plugins/voicechat/voicechat-server.properties"},
	// The mod build keeps its server config one level down (ModVoiceServer:
	// "config/" + modId + "/server"); the plugin build uses its data folder.
	{"plasmo", "config/plasmovoice/server/config.toml"},
	{"plasmo", "plugins/PlasmoVoice/config.toml"},
}

var (
	ErrVoiceModMissing   = errors.New("no voice chat mod config found")
	ErrVoiceModAmbiguous = errors.New("more than one voice chat mod config found")
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
		return "", "", fmt.Errorf("%w (checked %d paths)", ErrVoiceModMissing, len(voiceConfigFiles))
	case 1:
		return mod, path, nil
	default:
		return "", "", fmt.Errorf("%w: %s", ErrVoiceModAmbiguous, strings.Join(hits, ", "))
	}
}

// FindVoiceConfig reads every known voice config path of a sub-server and
// returns the one that exists. relPath is relative to the sub-server.
func FindVoiceConfig(reg *nodegrpc.Registry, nodeID int, serverUUID, subServer string) (mod, relPath, content string, err error) {
	contents := map[string]string{}
	var found []string
	for _, f := range voiceConfigFiles {
		c, ok, err := ReadNodeFileString(reg, nodeID, serverUUID, subServer+"/"+f.Path)
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
