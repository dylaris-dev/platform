package handlers

import (
	"context"
	"io"
	"log"
	"path"
	"strings"
	"time"

	"dylaris-core/models"
)

// A build's rendered objects live in one directory, modpacks/<hmac>/: the
// .mrpack, the server pack named by server.last, and that marker. Nothing
// removed them with the build, its pack or its owner's account, and the
// provider cannot list, so once the row was gone nothing could find them again.

// packBuildDirs returns the storage directories that hold a build's objects:
// the derived one, and the stored key's when a publish recorded another.
func (s *AppState) packBuildDirs(pack *models.Pack, build *models.PackBuild) []string {
	h := &PacksHandler{state: s}
	dirs := []string{strings.TrimSuffix(h.mrpackStorageKey(pack, build), "pack.mrpack")}
	if k := build.MrpackStorageKey; strings.HasPrefix(k, "modpacks/") && strings.HasSuffix(k, "/pack.mrpack") {
		if d := path.Dir(k) + "/"; d != dirs[0] {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// packDirsOfPack collects the directories of every build of a pack. Read BEFORE
// the rows are deleted; afterwards there is nothing left to derive them from.
func (s *AppState) packDirsOfPack(pack *models.Pack) []string {
	builds, err := s.Store.ListPackBuilds(pack.ID)
	if err != nil {
		log.Printf("packs: cannot list builds of pack %d for storage cleanup: %v", pack.ID, err)
		return nil
	}
	var dirs []string
	for i := range builds {
		dirs = append(dirs, s.packBuildDirs(pack, &builds[i])...)
	}
	return dirs
}

// PackDirsOfUser collects the directories of every build the account owns.
func (s *AppState) PackDirsOfUser(userID string) []string {
	if s == nil || s.Store == nil || userID == "" {
		return nil
	}
	// Without a modpack storage nothing can be deleted, so nothing is listed.
	if prov, err := s.buildModpackStorageProvider(); err != nil || prov == nil {
		return nil
	}
	packs, err := s.Store.ListPacksByOwner(userID)
	if err != nil {
		log.Printf("packs: cannot list packs of %s for storage cleanup: %v", userID, err)
		return nil
	}
	var dirs []string
	for i := range packs {
		dirs = append(dirs, s.packDirsOfPack(&packs[i])...)
	}
	return dirs
}

// DropPackDirs removes the objects in the given build directories. Best effort:
// called after the rows are gone, so a failure costs storage, never a build.
func (s *AppState) DropPackDirs(dirs []string) {
	if len(dirs) == 0 {
		return
	}
	prov, err := s.buildModpackStorageProvider()
	if err != nil {
		log.Printf("packs: cannot open modpack storage to clean %d build directories: %v", len(dirs), err)
		return
	}
	if prov == nil {
		return // no modpack storage configured: nothing was ever stored
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, dir := range dirs {
		if !strings.HasPrefix(dir, "modpacks/") || !strings.HasSuffix(dir, "/") {
			continue
		}
		marker := dir + "server.last"
		if rc, _, err := prov.Stream(ctx, marker); err == nil {
			prev, _ := io.ReadAll(io.LimitReader(rc, 512))
			rc.Close()
			if p := string(prev); strings.HasPrefix(p, dir+"server-") {
				_ = prov.Delete(ctx, p)
			}
		}
		for _, k := range []string{marker, dir + "pack.mrpack"} {
			if err := prov.Delete(ctx, k); err != nil {
				log.Printf("packs: could not delete %s: %v", k, err)
			}
		}
	}
}
