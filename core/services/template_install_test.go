package services

import (
	"testing"

	"dylaris-core/models"
)

func TestTemplateInstallPlan(t *testing.T) {
	cases := []struct {
		name                                   string
		tmpl                                   models.Template
		version, mcVersion, loader, wantErrMsg string
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
		{name: "a source that is not server software", tmpl: models.Template{Software: "modpack"}, wantErrMsg: `template software "modpack" cannot be installed`},
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
			if typ != "upload-zip" || sw != c.tmpl.Software || v != c.version || mc != c.mcVersion || loader != c.loader {
				t.Fatalf("plan = %q %q version %q mc %q loader %q, want upload-zip %q %q %q %q",
					typ, sw, v, mc, loader, c.tmpl.Software, c.version, c.mcVersion, c.loader)
			}
		})
	}
}
