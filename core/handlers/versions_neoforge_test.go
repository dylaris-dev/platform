package handlers

import "testing"

func TestNeoForgeMajor(t *testing.T) {
	tests := []struct {
		build string
		want  string
		ok    bool
	}{
		{"21.1.262", "1.21", true},
		{"20.4.237", "1.20", true},
		// Since MC 26.x the build follows Minecraft itself; the old rule said "1.26".
		{"26.3.0.89", "26.3", true},
		{"26.1.0.19", "26.1", true},
		// Pre-releases are not offered, like PaperMC's.
		{"26.1.0.19-beta", "", false},
		{"26.1.0.0-alpha.4+snapshot-1", "", false},
		// The April-fools builds made a Minecraft "1.0" appear.
		{"0.25w14craftmine.3", "", false},
		{"garbage", "", false},
	}
	for _, tt := range tests {
		got, ok := neoForgeMajor(tt.build)
		if got != tt.want || ok != tt.ok {
			t.Errorf("neoForgeMajor(%q) = %q, %v; want %q, %v", tt.build, got, ok, tt.want, tt.ok)
		}
	}
}

// The same cases as database.TestIntegrationNeoForgeMinecraftSQL, which pins
// the backfill's SQL twin of this mapping.
func TestNeoForgeMinecraft(t *testing.T) {
	for _, tt := range []struct{ loader, want string }{
		{"21.1.264", "1.21.1"},
		{"21.0.5", "1.21"},
		{"21.10.3", "1.21.10"},
		{"20.4.237", "1.20.4"},
		{"20.2.86", "1.20.2"},
		{"26.1.2.5", "26.1.2"},
		{"26.2.0.7", "26.2"},
		{"26.3.0.89", "26.3"},
		{" 21.1.264 ", "1.21.1"},
		{"21.1.0-beta", ""},
		{"26.1.0.0-alpha.4+snapshot-1", ""},
		{"0.25w14craftmine.3", ""},
		{"0.1.2", ""},
		{"21.1", ""},
		{"", ""},
		{"garbage", ""},
		{"99999999999.1.2", ""},
	} {
		if got := neoForgeMinecraft(tt.loader); got != tt.want {
			t.Errorf("neoForgeMinecraft(%q) = %q, want %q", tt.loader, got, tt.want)
		}
	}
}

// What a setup or reinstall records: the live case was a NeoForge 21.1.264
// server recorded with no Minecraft version, and Forge servers recorded with
// the line "1.21" while running 1.21.1.
func TestApplyExactMinecraftVersion(t *testing.T) {
	for _, tt := range []struct {
		name                      string
		software, mc, ver, loader string
		wantMC, wantVer           string
	}{
		{"neoforge from the loader", "neoforge", "", "", "21.1.264", "1.21.1", "21.1.264"},
		{"neoforge overrides a line", "neoforge", "1.21", "", "21.1.264", "1.21.1", "21.1.264"},
		{"neoforge unparseable loader keeps mc", "neoforge", "1.21.1", "", "21.1.0-beta", "1.21.1", "21.1.0-beta"},
		{"forge version extends the line", "forge", "1.21", "1.21.1", "", "1.21.1", "1.21.1"},
		{"paper exact already", "paper", "1.21.4", "1.21.4", "", "1.21.4", "1.21.4"},
		{"fabric no mc sent", "fabric", "", "1.21.1", "", "1.21.1", "1.21.1"},
		{"vanilla conflicting pair kept", "vanilla", "1.20", "1.21.1", "", "1.20", "1.21.1"},
		{"line prefix is not a dot boundary", "paper", "1.2", "1.21.1", "", "1.2", "1.21.1"},
		{"modpack untouched", "modpack", "1.21", "abc", "fabric", "1.21", "abc"},
		{"technic untouched", "technic", "1.12.2", "1.0.5", "", "1.12.2", "1.0.5"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mc, ver := tt.mc, tt.ver
			applyExactMinecraftVersion(&mc, &ver, tt.software, tt.loader)
			if mc != tt.wantMC || ver != tt.wantVer {
				t.Errorf("got mc %q build %q, want %q %q", mc, ver, tt.wantMC, tt.wantVer)
			}
		})
	}
}
