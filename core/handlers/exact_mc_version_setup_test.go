package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
)

// Setup records the EXACT Minecraft version, whatever the panel sent: the live
// case was NeoForge 21.1.264 recorded with none and Forge recorded as "1.21".
func TestSetupRecordsTheExactMinecraftVersion(t *testing.T) {
	for _, tt := range []struct {
		name      string
		installer map[string]string
		wantMC    string
		wantBuild string
	}{
		{"neoforge sends only the loader", map[string]string{"type": "neoforge", "loader": "21.1.264"}, "1.21.1", "21.1.264"},
		{"forge sends the line", map[string]string{"type": "forge", "version": "1.21.1", "mcVersion": "1.21"}, "1.21.1", "1.21.1"},
		{"upload with neoforge", map[string]string{"type": "upload-zip", "structure": "direct", "software": "neoforge", "loader": "21.1.264"}, "1.21.1", "21.1.264"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, fs, rdb := newInstallTest(t)
			rec := httptest.NewRecorder()
			h.SetupServer(rec, installRequest("setup", setupBody(tt.installer), false))
			if rec.Code != http.StatusOK {
				t.Fatalf("setup status %d: %s", rec.Code, rec.Body.String())
			}
			if fs.setupMC != tt.wantMC || fs.setupBuild != tt.wantBuild {
				t.Errorf("server row = %q/%q, want %q/%q", fs.setupMC, fs.setupBuild, tt.wantMC, tt.wantBuild)
			}
			if fs.installRow.McVersion != tt.wantMC || fs.installRow.BuildVersion != tt.wantBuild {
				t.Errorf("sub-server row = %q/%q, want %q/%q", fs.installRow.McVersion, fs.installRow.BuildVersion, tt.wantMC, tt.wantBuild)
			}
			if inst := queuedInstaller(t, rdb); len(inst) != 1 || inst[0]["loader"] != tt.installer["loader"] {
				t.Fatalf("queued %v, want the loader passed through", inst)
			}
		})
	}
}

// A NeoForge reinstall naming no loader reinstalls the recorded one and
// records its Minecraft version. It used to be refused with a 400.
func TestReinstallNeoForgeUsesTheRecordedLoader(t *testing.T) {
	h, fs, rdb := newInstallTest(t)
	fs.server.Status = "stopped"
	fs.server.InstallerType = "neoforge"
	fs.server.MinecraftVersion = ""
	fs.server.BuildNumber = ""
	fs.storedInstall = &models.SubServerInstall{ServerID: 1, SubServerName: "main", InstallerType: "neoforge", Loader: "21.1.264"}

	rec := httptest.NewRecorder()
	h.ReinstallServer(rec, installRequest("reinstall", map[string]any{"javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest"}, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("reinstall status %d: %s", rec.Code, rec.Body.String())
	}
	if fs.setupMC != "1.21.1" || fs.setupBuild != "21.1.264" {
		t.Errorf("server row = %q/%q, want 1.21.1/21.1.264", fs.setupMC, fs.setupBuild)
	}
	if inst := queuedInstaller(t, rdb); len(inst) != 1 || inst[0]["type"] != "neoforge" || inst[0]["loader"] != "21.1.264" {
		t.Fatalf("queued %v, want neoforge with the recorded loader", inst)
	}

	// Nothing recorded to fall back on: still refused.
	h2, fs2, _ := newInstallTest(t)
	fs2.server.Status = "stopped"
	fs2.server.InstallerType = "neoforge"
	fs2.server.BuildNumber = ""
	rec = httptest.NewRecorder()
	h2.ReinstallServer(rec, installRequest("reinstall", map[string]any{"javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest"}, true))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reinstall without any loader: status %d, want 400", rec.Code)
	}
}

// A reinstall naming a new exact version records it, not the stale stored one.
func TestReinstallRecordsTheRequestedVersion(t *testing.T) {
	h, fs, _ := newInstallTest(t)
	fs.server.Status = "stopped"
	rec := httptest.NewRecorder()
	h.ReinstallServer(rec, installRequest("reinstall", map[string]any{
		"javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest",
		"installer": map[string]string{"type": "paper", "version": "1.21.4"},
	}, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("reinstall status %d: %s", rec.Code, rec.Body.String())
	}
	if fs.setupMC != "1.21.4" {
		t.Errorf("minecraft_version = %q, want 1.21.4", fs.setupMC)
	}
}
