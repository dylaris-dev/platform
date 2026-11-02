package services

import (
	"strings"
	"testing"

	"dylaris-core/models"
)

func TestTemplateInstallPlan(t *testing.T) {
	cases := []struct {
		name                                             string
		tmpl                                             models.Template
		software, version, mcVersion, loader, wantErrMsg string
	}{
		{name: "paper", tmpl: models.Template{Software: "paper", McVersion: "1.21.4", SoftwareVersion: "1.21.4"}, version: "1.21.4", mcVersion: "1.21.4"},
		{name: "paper without a software version installs the Minecraft version", tmpl: models.Template{Software: "paper", McVersion: "1.21.4"}, version: "1.21.4", mcVersion: "1.21.4"},
		{name: "vanilla", tmpl: models.Template{Software: "vanilla", McVersion: "1.21.1", SoftwareVersion: "1.21.1"}, version: "1.21.1", mcVersion: "1.21.1"},
		{name: "velocity", tmpl: models.Template{Software: "velocity", SoftwareVersion: "3.4.0"}, version: "3.4.0"},
		{name: "waterfall", tmpl: models.Template{Software: "waterfall", McVersion: "1.21", SoftwareVersion: "1.21"}, version: "1.21", mcVersion: "1.21"},
		{name: "bungeecord", tmpl: models.Template{Software: "bungeecord", SoftwareVersion: "#2104"}, version: "#2104"},
		{name: "neoforge", tmpl: models.Template{Software: "neoforge", McVersion: "1.21.1", SoftwareVersion: "21.1.264"}, mcVersion: "1.21.1", loader: "21.1.264"},
		{name: "fabric with a loader", tmpl: models.Template{Software: "fabric", McVersion: "1.21.4", SoftwareVersion: "0.16.10"}, version: "1.21.4", mcVersion: "1.21.4", loader: "0.16.10"},
		{name: "forge with a build", tmpl: models.Template{Software: "forge", McVersion: "1.20.1", SoftwareVersion: "47.3.0"}, version: "1.20.1", mcVersion: "1.20.1", loader: "47.3.0"},
		// Captured from an online install, whose build IS the Minecraft version.
		{name: "fabric captured from an online install", tmpl: models.Template{Software: "fabric", McVersion: "1.21.4", SoftwareVersion: "1.21.4"}, version: "1.21.4", mcVersion: "1.21.4"},
		{name: "empty software", tmpl: models.Template{McVersion: "1.21.4"}, wantErrMsg: "template has no server software set"},
		{name: "a source that is not server software", tmpl: models.Template{Software: "modpack"}, wantErrMsg: `template cannot be installed: software "modpack" is not supported; use one of paper, vanilla, fabric, forge, neoforge, velocity, waterfall, bungeecord`},
		// Saved before saves were validated: normalised at install too.
		{name: "capitalised software", tmpl: models.Template{Software: "Forge", McVersion: "1.20.1", SoftwareVersion: "47.3.0"}, software: "forge", version: "1.20.1", mcVersion: "1.20.1", loader: "47.3.0"},
		{name: "forge full version is split, not prefixed twice", tmpl: models.Template{Software: "forge", McVersion: "1.20.1", SoftwareVersion: "1.20.1-47.3.0"}, version: "1.20.1", mcVersion: "1.20.1", loader: "47.3.0"},
		{name: "forge full version without a Minecraft version", tmpl: models.Template{Software: "forge", SoftwareVersion: "1.20.1-47.3.0"}, version: "1.20.1", mcVersion: "1.20.1", loader: "47.3.0"},
		{name: "a paper build never goes out as the Minecraft version", tmpl: models.Template{Software: "paper", McVersion: "1.21.4", SoftwareVersion: "232"}, version: "1.21.4", mcVersion: "1.21.4"},
		{name: "paper with only a build", tmpl: models.Template{Software: "paper", SoftwareVersion: "232"}, wantErrMsg: "template cannot be installed: a paper template needs the Minecraft version, such as 1.21.4 (the newest paper build of it is installed)"},
		{name: "neoforge given a Minecraft version", tmpl: models.Template{Software: "neoforge", McVersion: "1.21.1", SoftwareVersion: "1.21.1"}, wantErrMsg: `template cannot be installed: "1.21.1" is a Minecraft version, not a NeoForge version; NeoForge versions look like 21.1.265 (for Minecraft 1.21.1)`},
		{name: "purpur", tmpl: models.Template{Software: "purpur", McVersion: "1.21.4"}, wantErrMsg: "template cannot be installed: purpur is not supported; use paper, which runs the same plugins"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			typ, sw, v, mc, loader, err := TemplateInstallPlan(c.tmpl)
			if c.wantErrMsg != "" {
				if err == nil || err.Error() != c.wantErrMsg {
					t.Fatalf("err = %v, want %q", err, c.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantSW := c.software
			if wantSW == "" {
				wantSW = c.tmpl.Software
			}
			if typ != "upload-zip" || sw != wantSW || v != c.version || mc != c.mcVersion || loader != c.loader {
				t.Fatalf("plan = %q %q version %q mc %q loader %q, want upload-zip %q %q %q %q",
					typ, sw, v, mc, loader, wantSW, c.version, c.mcVersion, c.loader)
			}
		})
	}
}

// A template is checked when it is saved with the rules its install applies,
// and stored in the form the install reads.
func TestValidateTemplateSoftware(t *testing.T) {
	cases := []struct {
		name            string
		in              models.Template
		sw, mc, sv, err string
	}{
		{name: "no software is files only", in: models.Template{McVersion: "1.21.4"}, mc: "1.21.4"},
		{name: "lower-cased and trimmed", in: models.Template{Software: " Paper ", McVersion: "1.21.4"}, sw: "paper", mc: "1.21.4"},
		{name: "paper build dropped", in: models.Template{Software: "paper", McVersion: "1.21.4", SoftwareVersion: "232"}, sw: "paper", mc: "1.21.4"},
		{name: "paper version moved to Minecraft", in: models.Template{Software: "vanilla", SoftwareVersion: "1.21.4"}, sw: "vanilla", mc: "1.21.4"},
		{name: "forge full version split", in: models.Template{Software: "forge", SoftwareVersion: "1.20.1-47.3.0"}, sw: "forge", mc: "1.20.1", sv: "47.3.0"},
		{name: "forge full version for another Minecraft", in: models.Template{Software: "forge", McVersion: "1.21.1", SoftwareVersion: "1.20.1-47.3.0"}, err: "forge 1.20.1-47.3.0 is for Minecraft 1.20.1, not 1.21.1"},
		{name: "forge build that is a Minecraft version", in: models.Template{Software: "forge", McVersion: "1.20.1", SoftwareVersion: "1.20.4"}, err: `"1.20.4" is not a Forge build; builds look like 47.3.0, or leave it empty for the recommended one`},
		{name: "forge without a Minecraft version", in: models.Template{Software: "forge", SoftwareVersion: "47.3.0"}, err: "a forge template needs the Minecraft version, such as 1.21.4"},
		{name: "forge build equal to Minecraft means newest", in: models.Template{Software: "forge", McVersion: "1.20.1", SoftwareVersion: "1.20.1"}, sw: "forge", mc: "1.20.1", sv: "1.20.1"},
		{name: "fabric loader", in: models.Template{Software: "fabric", McVersion: "1.21.4", SoftwareVersion: "0.16.10"}, sw: "fabric", mc: "1.21.4", sv: "0.16.10"},
		{name: "fabric loader nonsense", in: models.Template{Software: "fabric", McVersion: "1.21.4", SoftwareVersion: "latest"}, err: `"latest" is not a Fabric loader version; they look like 0.16.10, or leave it empty for the newest`},
		{name: "neoforge fills the Minecraft version", in: models.Template{Software: "neoforge", SoftwareVersion: "21.1.265"}, sw: "neoforge", mc: "1.21.1", sv: "21.1.265"},
		{name: "neoforge 26 four-part", in: models.Template{Software: "neoforge", SoftwareVersion: "26.1.0.5"}, sw: "neoforge", mc: "26.1", sv: "26.1.0.5"},
		{name: "neoforge beta", in: models.Template{Software: "neoforge", McVersion: "1.21", SoftwareVersion: "21.0.0-beta"}, sw: "neoforge", mc: "1.21", sv: "21.0.0-beta"},
		{name: "neoforge given a 26 Minecraft version", in: models.Template{Software: "neoforge", SoftwareVersion: "26.1"}, err: `"26.1" is a Minecraft version, not a NeoForge version; NeoForge versions look like 21.1.265 (for Minecraft 1.21.1)`},
		{name: "neoforge without a version", in: models.Template{Software: "neoforge", McVersion: "1.21.1"}, err: "a neoforge template needs the NeoForge version, such as 21.1.265 for Minecraft 1.21.1"},
		{name: "quilt", in: models.Template{Software: "quilt"}, err: `software "quilt" is not supported; use one of paper, vanilla, fabric, forge, neoforge, velocity, waterfall, bungeecord`},
		{name: "velocity snapshot refused like an install", in: models.Template{Software: "velocity", SoftwareVersion: "3.4.0-SNAPSHOT"}, err: "installer.version must be a velocity release such as 3.5.1"},
		{name: "bungeecord without a build", in: models.Template{Software: "bungeecord"}, err: "installer.version is required for bungeecord"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in
			err := ValidateTemplateSoftware(&got)
			if c.err != "" {
				if err == nil || err.Error() != c.err {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				if got != c.in {
					t.Fatalf("a refused template was changed: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Software != c.sw || got.McVersion != c.mc || got.SoftwareVersion != c.sv {
				t.Fatalf("got %q %q %q, want %q %q %q", got.Software, got.McVersion, got.SoftwareVersion, c.sw, c.mc, c.sv)
			}
		})
	}
}

func TestTemplateLoaderFromLibraries(t *testing.T) {
	cases := []struct {
		name, software, mc string
		dirs               []string
		want               string
	}{
		{"forge", "forge", "1.20.1", []string{"1.20.1-47.4.28"}, "47.4.28"},
		{"forge of another Minecraft ignored", "forge", "1.20.1", []string{"1.19.2-43.3.0", "1.20.1-47.4.28"}, "47.4.28"},
		{"forge leftovers pin nothing", "forge", "1.20.1", []string{"1.20.1-47.3.0", "1.20.1-47.4.28"}, ""},
		{"neoforge", "neoforge", "1.21.1", []string{"21.1.265"}, "21.1.265"},
		{"fabric", "fabric", "1.21.4", []string{"0.20.1"}, "0.20.1"},
		{"nothing installed", "fabric", "1.21.4", nil, ""},
	}
	for _, c := range cases {
		if got := TemplateLoaderFromLibraries(c.software, c.mc, c.dirs); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTemplatePrivacyPaths(t *testing.T) {
	got := TemplatePrivacyPaths([]string{"logs", "world", "proxy.log.0", "crash-reports", "usercache.json", "plugins/x/logs", "config", "latest.log", "logback.xml"})
	want := []string{"logs", "proxy.log.0", "crash-reports", "usercache.json", "plugins/x/logs", "latest.log"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}
