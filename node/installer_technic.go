package main

import (
	"archive/zip"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Technic Platform packs. Core resolves the pack and sends one of three
// variants; nothing here talks to the Technic API.
//
//	server         URL is the author's server pack: a finished server directory.
//	client-zip     URL is the client pack zip; the server is rebuilt from it.
//	client-solder  TechnicMods are the Solder build's mod zips; same rebuild.
//
// Every download goes through the guarded client (installer_guard.go): the
// author picks these hosts, and our nodes share a network with Redis.

// TechnicMod is one Solder mod archive.
type TechnicMod struct {
	URL string `json:"url"`
	MD5 string `json:"md5"`
}

const (
	technicPackMax = 2 << 30 // one pack archive
	technicModMax  = 1 << 30 // one Solder mod archive
	// technicUnpackMax bounds what one install may unpack in total, read from
	// the archives' headers before anything is extracted. archive/zip refuses
	// an entry that inflates past its declared size, so the header is binding.
	// A download cap alone let a 2 GiB zip bomb fill the node's disk.
	technicUnpackMax = 16 << 30
	// technicModsMax bounds a Solder build's file list; real builds list a few
	// hundred.
	technicModsMax = 2000
)

// The loader installers and the in-container installer run, as variables so a
// test can record the call instead of reaching Maven and Docker.
var (
	technicInstallForge    = installForge
	technicInstallNeoForge = installNeoForge
	technicInstallFabric   = installFabric
	technicRunInstaller    = runJavaInstaller
)

func installTechnic(destDir string, cfg InstallerConfig) error {
	switch cfg.Variant {
	case "server", "client-zip":
		if err := checkTechnicURL(cfg.URL); err != nil {
			return err
		}
	case "client-solder":
		if len(cfg.TechnicMods) == 0 {
			return errors.New("the Solder build lists no files")
		}
		if len(cfg.TechnicMods) > technicModsMax {
			return fmt.Errorf("the Solder build lists %d files, more than the %d allowed", len(cfg.TechnicMods), technicModsMax)
		}
		for _, m := range cfg.TechnicMods {
			if err := checkTechnicURL(m.URL); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown Technic pack variant %q", cfg.Variant)
	}

	// Created fresh with a random name: destDir is writable by the tenant, so a
	// fixed name could already be a symlink pointing somewhere else.
	stage, err := os.MkdirTemp(destDir, ".technic-stage-")
	if err != nil {
		return fmt.Errorf("preparing the install failed: %w", err)
	}
	defer os.RemoveAll(stage)
	dl, err := os.MkdirTemp(destDir, ".technic-dl-")
	if err != nil {
		return fmt.Errorf("preparing the install failed: %w", err)
	}
	defer os.RemoveAll(dl)

	budget := int64(technicUnpackMax)
	if cfg.Variant == "client-solder" {
		for i, m := range cfg.TechnicMods {
			if err := fetchTechnicArchive(m.URL, m.MD5, filepath.Join(dl, fmt.Sprintf("%d.zip", i)), technicModMax, stage, &budget); err != nil {
				return err
			}
		}
	} else if err := fetchTechnicArchive(cfg.URL, "", filepath.Join(dl, "pack.zip"), technicPackMax, stage, &budget); err != nil {
		return err
	}

	// Every move into the server directory goes through an os.Root: the
	// tenant can write there while the install runs, and a path checked first
	// and renamed later can meet a symlink swapped in between. Root refuses
	// any step that leaves the directory, at the moment of the step.
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return fmt.Errorf("opening the server directory failed: %w", err)
	}
	defer root.Close()
	stageName := filepath.Base(stage)

	if cfg.Variant == "server" {
		if err := liftSoleDirectory(stage); err != nil {
			return err
		}
		if err := mergeStage(root, stageName, "."); err != nil {
			return err
		}
		return makeTechnicServerLaunchable(destDir, cfg)
	}

	loader, err := detectTechnicLoader(stage)
	if err != nil {
		return err
	}
	log.Printf("Technic client pack: %s %s for Minecraft %s", loader.Kind, loader.Version, loader.MC)
	switch loader.Kind {
	case "forge":
		err = technicInstallForge(destDir, loader.MC, loader.Version, cfg.JavaImage, cfg.ServerUUID)
	case "neoforge":
		err = technicInstallNeoForge(destDir, loader.Version, cfg.JavaImage, cfg.ServerUUID)
	case "fabric":
		err = technicInstallFabric(destDir, loader.MC, loader.Version)
	}
	if err != nil {
		return fmt.Errorf("installing %s for the pack failed: %w", loader.Kind, err)
	}
	// bin/ holds the CLIENT loader; the server one was just installed.
	if err := os.RemoveAll(filepath.Join(stage, "bin")); err != nil {
		return err
	}
	return mergeStage(root, stageName, ".")
}

func checkTechnicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("the pack download address is not an http(s) URL: %q", raw)
	}
	return nil
}

// fetchTechnicArchive downloads one archive, checks its md5 when the source
// published one, and extracts it into stage.
func fetchTechnicArchive(rawURL, wantMD5, dst string, max int64, stage string, budget *int64) error {
	name := filepath.Base(strings.SplitN(rawURL, "?", 2)[0])
	log.Printf("Downloading %s ...", name)
	if err := downloadFileGuarded(rawURL, dst, max); err != nil {
		return fmt.Errorf("downloading %s failed: %w", name, err)
	}
	if wantMD5 != "" {
		got, err := fileMD5(dst)
		if err != nil {
			return err
		}
		if !strings.EqualFold(got, wantMD5) {
			return fmt.Errorf("%s does not match its published checksum", name)
		}
	}
	if err := spendUnpackBudget(dst, budget); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := extractZipToDir(dst, stage); err != nil {
		return fmt.Errorf("extracting %s failed: %w", name, err)
	}
	return os.Remove(dst)
}

func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// spendUnpackBudget charges an archive's declared uncompressed size against
// what this install may still unpack.
func spendUnpackBudget(zipPath string, budget *int64) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("not a readable zip archive: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		size := int64(f.UncompressedSize64)
		if size < 0 || size > *budget {
			return errors.New("the pack unpacks to more than the node allows for one install")
		}
		*budget -= size
	}
	return nil
}

// serverDirs are directories a server pack ships at its top level. A pack
// holding only one of them is not wrapped in a folder, it is that folder.
var serverDirs = map[string]bool{"mods": true, "config": true, "world": true, "libraries": true, "plugins": true}

// liftSoleDirectory moves the contents of dir/<x>/ up when <x> is the ONLY
// entry in dir. Stricter than upload-zip's "subfolder" on purpose: a server
// pack of Tekkit.jar plus mods/ must not have its mods pulled into the root.
func liftSoleDirectory(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != 1 || !entries[0].IsDir() || serverDirs[strings.ToLower(entries[0].Name())] {
		return nil
	}
	sub := filepath.Join(dir, entries[0].Name())
	inner, err := os.ReadDir(sub)
	if err != nil {
		return err
	}
	for _, e := range inner {
		if err := os.Rename(filepath.Join(sub, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return os.Remove(sub)
}

// mergeStage moves every entry of stageName/rel into rel, merging directories
// and replacing files, all through root (see installTechnic).
func mergeStage(root *os.Root, stageName, rel string) error {
	entries, err := fs.ReadDir(root.FS(), path.Join(stageName, rel))
	if err != nil {
		return err
	}
	for _, e := range entries {
		to := path.Join(rel, e.Name())
		// The node's own files (.active_server, the backup store, ...) are
		// never the pack's to replace.
		if isProtectedFile(to) {
			log.Printf("Technic pack: skipping protected path %s", to)
			continue
		}
		if e.IsDir() {
			if fi, err := root.Lstat(to); err == nil && fi.IsDir() {
				if err := mergeStage(root, stageName, to); err != nil {
					return err
				}
				continue
			}
		}
		if err := root.RemoveAll(to); err != nil {
			return fmt.Errorf("replacing %s failed: %w", to, err)
		}
		if err := root.Rename(path.Join(stageName, to), to); err != nil {
			return fmt.Errorf("placing %s failed: %w", to, err)
		}
	}
	return nil
}

// makeTechnicServerLaunchable makes sure resolveLaunch finds something to run
// in an extracted server pack. Packs ship either a ready jar under a name of
// the author's choosing (Tekkit Classic: Tekkit.jar), or only a Forge
// installer the owner is expected to run.
func makeTechnicServerLaunchable(dir string, cfg InstallerConfig) error {
	if resolveLaunch(dir).Mode != launchNone {
		return nil
	}
	installers, others := rootJars(dir)
	if len(installers) == 1 {
		log.Printf("Server pack ships only an installer, running %s", installers[0])
		if err := technicRunInstaller(cfg.ServerUUID, filepath.Base(dir), cfg.JavaImage, installers[0], "--installServer"); err != nil {
			return fmt.Errorf("running the pack's installer failed: %w", err)
		}
		if resolveLaunch(dir).Mode != launchNone {
			return nil
		}
		_, others = rootJars(dir)
	}
	if len(others) == 1 {
		log.Printf("Using %s as the server jar", others[0])
		return os.Rename(filepath.Join(dir, others[0]), filepath.Join(dir, "server.jar"))
	}
	return errors.New("the server pack contains no server jar that can be started")
}

// rootJars splits the jars at the top of dir into installers and candidates,
// leaving out the vanilla server jar that legacy Forge loads by name.
func rootJars(dir string) (installers, others []string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := e.Name()
		lower := strings.ToLower(n)
		if e.IsDir() || !strings.HasSuffix(lower, ".jar") {
			continue
		}
		switch {
		case strings.Contains(lower, "installer"):
			installers = append(installers, n)
		case strings.HasPrefix(lower, "minecraft_server"):
		default:
			others = append(others, n)
		}
	}
	return installers, others
}
