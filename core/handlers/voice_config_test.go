package handlers

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readVoiceFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "voice", name))
	if err != nil {
		t.Fatal(err)
	}
	// core.autocrlf may check the fixtures out with CRLF; the writers emit LF.
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

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
		{"no config at all is a missing mod", nil, "", "", errVoiceModMissing},
		{"one svc config is svc", []string{svcPlugin}, "svc", svcPlugin, nil},
		{"one plasmo config is plasmo", []string{plasmoPlugin}, "plasmo", plasmoPlugin, nil},
		{"the plasmo mod config is one level down", []string{"config/plasmovoice/server/config.toml"}, "plasmo", "config/plasmovoice/server/config.toml", nil},
		{"the plasmo mod folder root is not its server config", []string{"config/plasmovoice/config.toml"}, "", "", errVoiceModMissing},
		{"svc next to plasmo is ambiguous", []string{svcMod, plasmoPlugin}, "", "", errVoiceModAmbiguous},
		{"both svc paths are ambiguous", []string{svcMod, svcPlugin}, "", "", errVoiceModAmbiguous},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mod, path, err := detectVoiceMod(c.found)
			if !errors.Is(err, c.wantErr) || mod != c.wantMod || path != c.wantPath {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", mod, path, err, c.wantMod, c.wantPath, c.wantErr)
			}
			if c.wantErr == errVoiceModAmbiguous {
				for _, f := range c.found {
					if !strings.Contains(err.Error(), f) {
						t.Fatalf("error %q does not name %s", err, f)
					}
				}
			}
		})
	}
}

func TestSvcVoiceGolden(t *testing.T) {
	before := readVoiceFixture(t, "svc_before.properties")
	after := readVoiceFixture(t, "svc_after.properties")

	if got := svcVoiceEnable(before, "a.eu.dylaris.com", 26000); got != after {
		t.Fatalf("enable:\n%s\nwant:\n%s", got, after)
	}
	if got := svcVoiceDisable(after); got != before {
		t.Fatalf("disable did not give the default back:\n%s", got)
	}
	// A server that shared the game port gets the in-container port back.
	moved := strings.Replace(before, "port=24454", "port=-1", 1)
	if got := svcVoiceEnable(moved, "a.eu.dylaris.com", 26000); got != after {
		t.Fatalf("enable on port=-1:\n%s", got)
	}
}

func TestPlasmoVoiceGolden(t *testing.T) {
	def := readVoiceFixture(t, "plasmo_default.toml")
	after := readVoiceFixture(t, "plasmo_after.toml")
	existing := readVoiceFixture(t, "plasmo_existing_public.toml")
	disabled := readVoiceFixture(t, "plasmo_disabled.toml")

	if got := plasmoVoiceEnable(def, "a.eu.dylaris.com", 26000); got != after {
		t.Fatalf("enable on the default:\n%s\nwant:\n%s", got, after)
	}
	if got := plasmoVoiceEnable(existing, "a.eu.dylaris.com", 26000); got != after {
		t.Fatalf("enable on an active [host.public]:\n%s\nwant:\n%s", got, after)
	}
	if got := plasmoVoiceDisable(after); got != disabled {
		t.Fatalf("disable:\n%s\nwant:\n%s", got, disabled)
	}
	if !strings.Contains(disabled, "#[host.public]\n#ip = \"127.0.0.1\"\n#port = 0\n") {
		t.Fatal("the null block is no longer a comment")
	}
}

func TestPlasmoVoiceEnableWithoutHostAppendsBoth(t *testing.T) {
	in := "debug = false\n\n[voice]\nmtu_size = 1024\n"
	want := in + "\n[host]\nport = 24454\n\n[host.public]\nip = \"a.eu.dylaris.com\"\nport = 26000\n"
	if got := plasmoVoiceEnable(in, "a.eu.dylaris.com", 26000); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlasmoVoiceEnableInsertsAfterHostSubtables(t *testing.T) {
	in := "[host]\nport = 0\n\n[host.extra]\nx = 1\n\n[voice]\nmtu_size = 1024\n"
	want := "[host]\nport = 24454\n\n[host.extra]\nx = 1\n\n[host.public]\nip = \"a.eu.dylaris.com\"\nport = 26000\n\n[voice]\nmtu_size = 1024\n"
	if got := plasmoVoiceEnable(in, "a.eu.dylaris.com", 26000); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlasmoMergeIsIdempotent(t *testing.T) {
	for _, name := range []string{"plasmo_default.toml", "plasmo_after.toml", "plasmo_existing_public.toml"} {
		t.Run(name, func(t *testing.T) {
			in := readVoiceFixture(t, name)
			once := plasmoVoiceEnable(in, "a.eu.dylaris.com", 26000)
			if twice := plasmoVoiceEnable(once, "a.eu.dylaris.com", 26000); twice != once {
				t.Fatalf("second merge changed the file:\n%s", twice)
			}
			off := plasmoVoiceDisable(once)
			if again := plasmoVoiceDisable(off); again != off {
				t.Fatalf("second disable changed the file:\n%s", again)
			}
		})
	}
}
