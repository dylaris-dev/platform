package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shapes are cut down from real CurseForge server packs: they ship mods
// and configs and leave the loader to a start script the node never runs.
func TestUploadedServerPackGetsItsDeclaredLoader(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		want  loaderCall
	}{
		"ServerPackCreator variables.txt (BMC4)": {map[string]string{
			"variables.txt": "# MODLOADER and MODLOADER_VERSION same thing\nMINECRAFT_VERSION=1.20.1\nMODLOADER=Forge\nMODLOADER_VERSION=47.4.20\nJAVA_ARGS=\"\"\n",
			// BMC4 also ships a manifest.json that is a file list, not the
			// CurseForge shape.
			"manifest.json": `{"files":["config","mods"]}`,
			"mods/a.jar":    "mod",
		}, loaderCall{"forge", "1.20.1", "47.4.20"}},
		"ServerPackCreator Fabric (BMC1)": {map[string]string{
			"variables.txt": "MINECRAFT_VERSION=1.19.2\nMODLOADER=Fabric\nMODLOADER_VERSION=0.14.24\n",
		}, loaderCall{"fabric", "1.19.2", "0.14.24"}},
		"CurseForge manifest.json (Vault Hunters)": {map[string]string{
			"manifest.json": `{"minecraft":{"version":"1.18.2","modLoaders":[{"id":"forge-40.3.11","primary":true}]},"manifestType":"minecraftModpack"}`,
		}, loaderCall{"forge", "1.18.2", "40.3.11"}},
		"ATM start script, Forge": {map[string]string{
			"startserver.sh": "#!/bin/sh\nFORGE_VERSION=47.4.0\nINSTALLER=\"forge-1.20.1-$FORGE_VERSION-installer.jar\"\n",
		}, loaderCall{"forge", "1.20.1", "47.4.0"}},
		"ATM start script, NeoForge": {map[string]string{
			"startserver.sh": "#!/bin/sh\nNEOFORGE_VERSION=21.1.251\nINSTALLER=\"neoforge-$NEOFORGE_VERSION-installer.jar\"\n",
		}, loaderCall{"neoforge", "", "21.1.251"}},
	} {
		t.Run(name, func(t *testing.T) {
			calls := stubLoaders(t)
			data := t.TempDir()
			dir := filepath.Join(data, "main")
			os.MkdirAll(dir, 0o755)
			writeUploadZip(t, dir, tc.files)

			if _, err := InstallServer(data, "main", InstallerConfig{Type: "upload-zip", Structure: "direct"}); err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 1 || (*calls)[0] != tc.want {
				t.Fatalf("loader installs %v, want %v", *calls, tc.want)
			}
		})
	}
}

// ATM10 ships the NeoForge installer itself; with no declaration found it is run.
func TestUploadedServerPackRunsItsOwnInstaller(t *testing.T) {
	calls := stubLoaders(t)
	data := t.TempDir()
	dir := filepath.Join(data, "main")
	os.MkdirAll(dir, 0o755)
	writeUploadZip(t, dir, map[string]string{"neoforge-21.1.251-installer.jar": "jar", "mods/a.jar": "mod"})

	if _, err := InstallServer(data, "main", InstallerConfig{Type: "upload-zip", Structure: "direct"}); err == nil {
		// The stub installs nothing, so the start still finds no server.
		t.Fatal("an installer that produced no server was accepted")
	}
	if len(*calls) != 1 || (*calls)[0] != (loaderCall{"installer", "", "neoforge-21.1.251-installer.jar"}) {
		t.Fatalf("calls %v", *calls)
	}
}

// The declaration files are the tenant's: a version that is not a version
// never reaches a Maven URL, and a pack that declares nothing usable is
// refused with the way out rather than installed blind.
func TestUploadedServerPackWithoutAUsableDeclarationIsRefused(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"path in the version": {"variables.txt": "MINECRAFT_VERSION=1.20.1\nMODLOADER=Forge\nMODLOADER_VERSION=../../evil\n"},
		"unknown loader":      {"variables.txt": "MINECRAFT_VERSION=1.20.1\nMODLOADER=Quilt\nMODLOADER_VERSION=0.26.0\n"},
		// Prominence II's server file is a zip holding the real zip.
		"zip in a zip": {"Prominence_server_pack.zip": "PK", "readme.txt": "unzip me"},
	} {
		t.Run(name, func(t *testing.T) {
			calls := stubLoaders(t)
			data := t.TempDir()
			dir := filepath.Join(data, "main")
			os.MkdirAll(dir, 0o755)
			writeUploadZip(t, dir, files)

			_, err := InstallServer(data, "main", InstallerConfig{Type: "upload-zip", Structure: "direct"})
			if err == nil || !strings.Contains(err.Error(), "choose the server software") {
				t.Fatalf("want the refusal naming the way out, got %v", err)
			}
			if len(*calls) != 0 {
				t.Fatalf("installed %v", *calls)
			}
		})
	}
}

// An upload that already starts is left alone, whatever it declares.
func TestUploadedServerWithAJarInstallsNoLoader(t *testing.T) {
	calls := stubLoaders(t)
	data := t.TempDir()
	dir := filepath.Join(data, "main")
	os.MkdirAll(dir, 0o755)
	writeUploadZip(t, dir, map[string]string{"server.jar": "mine", "variables.txt": "MINECRAFT_VERSION=1.20.1\nMODLOADER=Forge\nMODLOADER_VERSION=47.4.20\n"})

	if _, err := InstallServer(data, "main", InstallerConfig{Type: "upload-zip", Structure: "direct"}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("installed %v over a server that starts", *calls)
	}
}
