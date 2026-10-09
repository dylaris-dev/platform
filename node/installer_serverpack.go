package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
)

// A modpack's server pack (CurseForge's "Server Pack" file, ServerPackCreator
// output) ships mods and configs but rarely an installed loader: it expects
// its start script to install one on first run, and the node never runs that
// script. Every pack declares its loader in one of three places, measured on
// ATM8/9/10, BMC1/4 and Vault Hunters:
//
//   - variables.txt (ServerPackCreator): MINECRAFT_VERSION, MODLOADER, MODLOADER_VERSION
//   - manifest.json (CurseForge format): minecraft.version + modLoaders[].id "forge-40.3.11"
//   - startserver.sh (ATM): FORGE_VERSION=43.2.14 with the Minecraft version in
//     the installer name, or NEOFORGE_VERSION=21.1.251

// packVersionRe bounds what reaches a Maven URL: these files are the tenant's.
var packVersionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.+_-]{0,63}$`)

var (
	scriptForgeRe    = regexp.MustCompile(`(?m)^FORGE_VERSION=["']?([^"'\s]+)`)
	scriptNeoForgeRe = regexp.MustCompile(`(?m)^NEOFORGE_VERSION=["']?([^"'\s]+)`)
	scriptForgeMCRe  = regexp.MustCompile(`forge-([0-9][0-9.]*)-\$\{?FORGE_VERSION`)
)

// packFileMax bounds each declaration file read; BMC4's manifest.json (a
// file list, not the CurseForge shape) is 400 KB.
const packFileMax = 4 << 20

// detectServerPackLoader reads the loader a server pack declares. Read through
// root: the upload is the tenant's, and a link in it must not reach outside.
func detectServerPackLoader(root *os.Root) (technicLoader, bool) {
	if b, ok := readPackFile(root, "variables.txt"); ok {
		vars := map[string]string{}
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && !strings.HasPrefix(k, "#") {
				vars[k] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
		if l, ok := packLoader(vars["MODLOADER"], vars["MINECRAFT_VERSION"], vars["MODLOADER_VERSION"]); ok {
			return l, true
		}
	}
	if b, ok := readPackFile(root, "manifest.json"); ok {
		var m struct {
			Minecraft struct {
				Version    string `json:"version"`
				ModLoaders []struct {
					ID      string `json:"id"`
					Primary bool   `json:"primary"`
				} `json:"modLoaders"`
			} `json:"minecraft"`
		}
		if json.Unmarshal(b, &m) == nil {
			for _, ml := range m.Minecraft.ModLoaders {
				kind, ver, _ := strings.Cut(ml.ID, "-")
				if l, ok := packLoader(kind, m.Minecraft.Version, ver); ok && (ml.Primary || len(m.Minecraft.ModLoaders) == 1) {
					return l, true
				}
			}
		}
	}
	for _, name := range []string{"startserver.sh", "start.sh", "run.sh"} {
		b, ok := readPackFile(root, name)
		if !ok {
			continue
		}
		if m := scriptNeoForgeRe.FindSubmatch(b); m != nil {
			if l, ok := packLoader("neoforge", "", string(m[1])); ok {
				return l, true
			}
		}
		if m := scriptForgeRe.FindSubmatch(b); m != nil {
			if mc := scriptForgeMCRe.FindSubmatch(b); mc != nil {
				if l, ok := packLoader("forge", string(mc[1]), string(m[1])); ok {
					return l, true
				}
			}
		}
	}
	return technicLoader{}, false
}

func packLoader(kind, mc, version string) (technicLoader, bool) {
	kind = strings.ToLower(kind)
	if !packVersionRe.MatchString(version) || (kind != "neoforge" && !packVersionRe.MatchString(mc)) {
		return technicLoader{}, false
	}
	switch kind {
	case "forge", "neoforge", "fabric":
		return technicLoader{Kind: kind, MC: mc, Version: version}, true
	}
	return technicLoader{}, false
}

func readPackFile(root *os.Root, name string) ([]byte, bool) {
	f, err := openRegularIn(root, name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, err := readAllMax(f, packFileMax)
	return b, err == nil
}

var errNoServerInUpload = errors.New("the upload has no server jar and declares no mod loader; " +
	"if it only holds another .zip, upload that one instead, or choose the server software and version to install")

// makeUploadLaunchable makes an extracted upload startable when it carries no
// server of its own: the loader its server pack declares, else the one
// installer or lone jar it ships (the Technic server-pack rule).
func makeUploadLaunchable(dir string, cfg InstallerConfig) error {
	if resolveLaunch(dir).Mode != launchNone {
		return nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	loader, ok := detectServerPackLoader(root)
	root.Close()
	if ok {
		log.Printf("Server pack declares %s %s for Minecraft %s", loader.Kind, loader.Version, loader.MC)
		return installPackLoader(dir, loader, cfg)
	}
	if err := makeTechnicServerLaunchable(dir, cfg); errors.Is(err, errNoServerJarInPack) {
		return errNoServerInUpload
	} else {
		return err
	}
}

func installPackLoader(dir string, loader technicLoader, cfg InstallerConfig) error {
	var err error
	switch loader.Kind {
	case "forge":
		err = technicInstallForge(dir, loader.MC, loader.Version, cfg.JavaImage, cfg.ServerUUID)
	case "neoforge":
		err = technicInstallNeoForge(dir, loader.Version, cfg.JavaImage, cfg.ServerUUID)
	case "fabric":
		err = technicInstallFabric(dir, loader.MC, loader.Version)
	}
	if err != nil {
		return fmt.Errorf("installing %s for the pack failed: %w", loader.Kind, err)
	}
	return nil
}
