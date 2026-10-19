package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Clearing stale files before an install.
//
// An install used to write straight on top of whatever was already in the
// sub-server directory. For a jar swap that is fine; for a modpack update it is
// not - the new pack's mods are ADDED to the old pack's mods, and a server that
// boots at all then boots with two versions of half its mod list. Every operator
// who hit it was told to delete the folders over SFTP by hand.
//
// The wire carries TOKENS, never paths. A request cannot name a filesystem path
// to delete, so it cannot name one that was not designed in - which matters
// because this is the one place the node destroys a tenant's files on somebody
// else's say-so. The tokens are resolved to paths here, and each resolved path
// is still put through the same containment guard the file API uses, so a
// planted symlink cannot turn "mods" into something outside the directory.

const (
	// WipeMods clears the mod directory. The one that matters for a pack update.
	WipeMods = "mods"
	// WipeConfig clears pack-shipped configuration. Named separately from mods
	// because keeping hand-tuned config across a pack update is a legitimate
	// choice, and a pack that renamed a config file is a legitimate reason not to.
	WipeConfig = "config"
	// WipeLibraries clears the loader's downloaded libraries. A loader or MC
	// version change leaves a tree that the new one neither uses nor overwrites.
	WipeLibraries = "libraries"
	// WipeVersions clears the vanilla version cache some loaders keep.
	WipeVersions = "versions"
	// WipeJars clears the server jars in the directory ROOT, non-recursively.
	// A version change leaves the old jar next to the new one, and which one
	// starts then depends on the start command rather than on what was chosen.
	WipeJars = "jars"
)

// wipeTargets maps a token to the directories it clears. Several per token is
// deliberate: "config" means the configuration a pack ships, and packs put it in
// more than one place.
var wipeTargets = map[string][]string{
	WipeMods:      {"mods"},
	WipeConfig:    {"config", "defaultconfigs"},
	WipeLibraries: {"libraries"},
	WipeVersions:  {"versions"},
	WipeJars:      nil, // handled separately: files, not a directory
}

// IsWipeToken reports whether a token is one this node knows how to clear.
func IsWipeToken(t string) bool {
	_, ok := wipeTargets[t]
	return ok
}

// WipeBeforeInstall clears what the tokens name, inside destDir.
//
// An UNKNOWN token is an error, not a shrug. Ignoring it would mean an operator
// who asked for a clean modpack update gets a dirty one and is told the install
// succeeded - which is the exact failure this exists to end, now with a
// confirmation dialog in front of it.
//
// A missing directory is not an error: "clear the mods" on a server that has
// none has already achieved what it asked for.
func WipeBeforeInstall(destDir string, tokens []string) error {
	for _, raw := range tokens {
		token := strings.TrimSpace(raw)
		if token == "" {
			continue
		}
		if !IsWipeToken(token) {
			return fmt.Errorf("unknown wipe target %q", token)
		}
		if token == WipeJars {
			if err := wipeRootJars(destDir); err != nil {
				return err
			}
			continue
		}
		for _, dir := range wipeTargets[token] {
			// The same guard the file API uses, symlink check included: the
			// server directory is bind-mounted into the tenant's own container,
			// so they can plant a link where a directory is expected and have
			// this follow it out of the tree.
			target, err := resolveWithinDir(destDir, dir)
			if err != nil {
				return fmt.Errorf("wipe %s: %w", dir, err)
			}
			if err := removeIfPresent(target); err != nil {
				return fmt.Errorf("wipe %s: %w", dir, err)
			}
		}
	}
	return nil
}

// wipeRootJars removes *.jar in the directory root, non-recursively.
//
// Non-recursive on purpose: a jar deeper in the tree is a mod or a library, and
// those are cleared by their own tokens or deliberately kept.
func wipeRootJars(destDir string) error {
	entries, err := os.ReadDir(destDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".jar") {
			continue
		}
		// A symlink named *.jar is reported by ReadDir as a non-directory. Remove
		// removes the LINK rather than its target, but the guard is kept anyway
		// so this path has no special case to reason about later.
		target, err := resolveWithinDir(destDir, e.Name())
		if err != nil {
			return fmt.Errorf("wipe jar %s: %w", e.Name(), err)
		}
		if err := removeIfPresent(target); err != nil {
			return fmt.Errorf("wipe jar %s: %w", e.Name(), err)
		}
	}
	return nil
}

func removeIfPresent(path string) error {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	log.Printf("install: clearing %s", path)
	return os.RemoveAll(path)
}

// loaderLaunchDirs hold the per-version unix_args.txt that resolveLaunch
// starts. A Forge or NeoForge install clears BOTH before its installer runs:
// resolveLaunch takes the first args file it finds, Forge before NeoForge, so
// a NeoForge install on top of a Forge one kept starting Forge (the client then
// says the server has no NeoForge), and two NeoForge versions started the
// alphabetically first. The installer rewrites its own dir; mods, config and
// the world are not in these.
var loaderLaunchDirs = []string{
	"libraries/net/minecraftforge/forge",
	"libraries/net/neoforged/neoforge",
}

func clearLoaderLaunchDirs(destDir string) error {
	for _, dir := range loaderLaunchDirs {
		target, err := resolveWithinDir(destDir, dir)
		if err != nil {
			return fmt.Errorf("clear %s: %w", dir, err)
		}
		if err := removeIfPresent(target); err != nil {
			return fmt.Errorf("clear %s: %w", dir, err)
		}
	}
	return nil
}
