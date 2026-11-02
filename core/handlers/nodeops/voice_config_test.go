package nodeops

import (
	"errors"
	"strings"
	"testing"
)

func TestDetectVoiceMod(t *testing.T) {
	svcMod := "config/voicechat/voicechat-server.properties"
	svcPlugin := "plugins/voicechat/voicechat-server.properties"
	plasmoPlugin := "plugins/PlasmoVoice/config.toml"
	cases := []struct {
		name     string
		found    []string
		wantMod  string
		wantPath string
		wantErr  error
	}{
		{"no config at all is a missing mod", nil, "", "", ErrVoiceModMissing},
		{"one svc config is svc", []string{svcPlugin}, "svc", svcPlugin, nil},
		{"one plasmo config is plasmo", []string{plasmoPlugin}, "plasmo", plasmoPlugin, nil},
		{"the plasmo mod config is one level down", []string{"config/plasmovoice/server/config.toml"}, "plasmo", "config/plasmovoice/server/config.toml", nil},
		{"the plasmo mod folder root is not its server config", []string{"config/plasmovoice/config.toml"}, "", "", ErrVoiceModMissing},
		{"svc next to plasmo is ambiguous", []string{svcMod, plasmoPlugin}, "", "", ErrVoiceModAmbiguous},
		{"both svc paths are ambiguous", []string{svcMod, svcPlugin}, "", "", ErrVoiceModAmbiguous},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mod, path, err := detectVoiceMod(c.found)
			if !errors.Is(err, c.wantErr) || mod != c.wantMod || path != c.wantPath {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", mod, path, err, c.wantMod, c.wantPath, c.wantErr)
			}
			if c.wantErr == ErrVoiceModAmbiguous {
				for _, f := range c.found {
					if !strings.Contains(err.Error(), f) {
						t.Fatalf("error %q does not name %s", err, f)
					}
				}
			}
		})
	}
}
