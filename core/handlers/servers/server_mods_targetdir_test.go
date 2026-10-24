package servers

import (
	"testing"

	"dylaris-core/models"
)

// The uninstall path used to recompute the mod's directory from the server's
// CURRENT installer_type. That is only the right answer while the loader has not
// changed since the install, and PATCH /api/servers/{id}/loader-metadata exists
// precisely so it can change - it is how an imported server declares its real
// loader. When it did, the removal was aimed at a directory the jar had never
// been in: the node deleted nothing, Core dropped the row anyway, and the jar
// kept loading with nothing in the UI left to remove it with.
func TestUninstallTargetDirUsesWhatTheInstallRecorded(t *testing.T) {
	cases := []struct {
		name          string
		recorded      string
		installerType string
		want          string
	}{
		{
			// The regression. Installed while the server was paper, so the jar is
			// in plugins/; the server has since been re-declared as fabric.
			name:          "a loader change after the install does not move the target",
			recorded:      "plugins",
			installerType: "fabric",
			want:          "plugins",
		},
		{
			name:          "and the same the other way round",
			recorded:      "mods",
			installerType: "paper",
			want:          "mods",
		},
		{
			name:          "recorded and derived agreeing is the ordinary case",
			recorded:      "plugins",
			installerType: "paper",
			want:          "plugins",
		},
		{
			// Rows written before the target_dir column existed. They were placed
			// by the derived value, so that is what they must be removed by.
			name:          "a pre-column row falls back to the loader",
			recorded:      "",
			installerType: "paper",
			want:          "plugins",
		},
		{
			name:          "a pre-column row on a mods loader",
			recorded:      "",
			installerType: "fabric",
			want:          "mods",
		},
		{
			// Never written by the install path, which validates the field, but
			// the fallback must not pass an unknown value through to a filepath
			// join on the node.
			name:          "a value that is neither mods nor plugins is not trusted",
			recorded:      "../../etc",
			installerType: "paper",
			want:          "plugins",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &models.ServerMod{TargetDir: c.recorded}
			if got := uninstallTargetDir(m, c.installerType); got != c.want {
				t.Errorf("uninstallTargetDir(%q, %q) = %q, want %q",
					c.recorded, c.installerType, got, c.want)
			}
		})
	}
}

// The install path is what fills target_dir, so the value it stores has to be
// one of the two the uninstall path will accept back. Guards against the two
// drifting apart: an install that recorded something else would send every later
// uninstall down the fallback and reintroduce the bug silently.
func TestInstallOnlyEverRecordsAnAcceptedTargetDir(t *testing.T) {
	for _, loader := range []string{"paper", "fabric", "forge", "velocity", "", "unknown"} {
		derived := defaultTargetDirForLoader(loader)
		m := &models.ServerMod{TargetDir: derived}
		// "fabric" here is a deliberately different loader: if the recorded value
		// were not accepted, the fallback would answer with that one instead.
		if got := uninstallTargetDir(m, "fabric"); got != derived {
			t.Errorf("install would record %q for loader %q, but uninstall reads it back as %q",
				derived, loader, got)
		}
	}
}
