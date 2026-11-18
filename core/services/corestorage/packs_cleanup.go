package corestorage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/storage/modpack"
)

// A build's rendered objects live in one directory, modpacks/<hmac>/: the
// .mrpack and, from before server packs became ephemeral, the server pack named
// by server.last and that marker. Nothing
// removed them with the build, its pack or its owner's account, and the
// provider cannot list, so once the row was gone nothing could find them again.

// PackBuildDirs returns the storage directories that hold a build's objects:
// the derived one, and the stored key's when a publish recorded another.
func (s *Manager) PackBuildDirs(pack *models.Pack, build *models.PackBuild) []string {
	dirs := []string{strings.TrimSuffix(s.MrpackStorageKey(pack, build), "pack.mrpack")}
	if k := build.MrpackStorageKey; strings.HasPrefix(k, "modpacks/") && strings.HasSuffix(k, "/pack.mrpack") {
		if d := path.Dir(k) + "/"; d != dirs[0] {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// PackBuildObjects is what deleting a build frees: its directories plus the
// storage keys of its content (uploads, fork copies). Read BEFORE the rows go.
// DropPackDirs deletes a content key only once no modversion points at it.
func (s *Manager) PackBuildObjects(pack *models.Pack, build *models.PackBuild) []string {
	objs := s.PackBuildDirs(pack, build)
	keys, err := s.Store.ListBuildContentStorageKeys(build.ID)
	if err != nil {
		log.Printf("packs: cannot list content of build %d for storage cleanup: %v", build.ID, err)
	}
	return append(objs, keys...)
}

// PackDirsOfPack collects the objects of every build of a pack. Read BEFORE
// the rows are deleted; afterwards there is nothing left to derive them from.
func (s *Manager) PackDirsOfPack(pack *models.Pack) []string {
	builds, err := s.Store.ListPackBuilds(pack.ID)
	if err != nil {
		log.Printf("packs: cannot list builds of pack %d for storage cleanup: %v", pack.ID, err)
		return nil
	}
	var dirs []string
	for i := range builds {
		dirs = append(dirs, s.PackBuildObjects(pack, &builds[i])...)
	}
	return dirs
}

// PackDirsOfUser collects the directories of every build the account owns.
func (s *Manager) PackDirsOfUser(userID string) []string {
	if s == nil || s.Store == nil || userID == "" {
		return nil
	}
	// Without a modpack storage nothing can be deleted, so nothing is listed.
	if prov, err := s.BuildModpackProvider(); err != nil || prov == nil {
		return nil
	}
	packs, err := s.Store.ListPacksByOwner(userID)
	if err != nil {
		log.Printf("packs: cannot list packs of %s for storage cleanup: %v", userID, err)
		return nil
	}
	var dirs []string
	for i := range packs {
		dirs = append(dirs, s.PackDirsOfPack(&packs[i])...)
	}
	// Also content no build holds any more; mods cascade with the account.
	keys, err := s.Store.ListOwnerContentStorageKeys(userID)
	if err != nil {
		log.Printf("packs: cannot list content of %s for storage cleanup: %v", userID, err)
	}
	return append(dirs, keys...)
}

// DropPackDirs removes the objects in the given build directories, and the
// given content keys no modversion references any more. Best effort: called
// after the rows are gone, so a failure costs storage, never a build.
func (s *Manager) DropPackDirs(dirs []string) {
	if len(dirs) == 0 {
		return
	}
	prov, err := s.BuildModpackProvider()
	if err != nil {
		log.Printf("packs: cannot open modpack storage to clean %d build directories: %v", len(dirs), err)
		return
	}
	if prov == nil {
		return // no modpack storage configured: nothing was ever stored
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	seen := map[string]bool{}
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if strings.HasPrefix(dir, "packs/") && !strings.HasSuffix(dir, "/") {
			// A content object can have more than one row (a migrated build's
			// copy shares the key); on a count error it stays.
			if n, err := s.Store.CountModversionsByStorageKey(dir); err != nil || n > 0 {
				continue
			}
			if err := prov.Delete(ctx, dir); err != nil {
				log.Printf("packs: could not delete %s: %v", dir, err)
			}
			continue
		}
		if !strings.HasPrefix(dir, "modpacks/") || !strings.HasSuffix(dir, "/") {
			continue
		}
		if err := dropStoredServerPack(ctx, prov, dir); err != nil {
			log.Printf("packs: %v", err)
		}
		if err := prov.Delete(ctx, dir+"pack.mrpack"); err != nil {
			log.Printf("packs: could not delete %spack.mrpack: %v", dir, err)
		}
	}
}

// dropStoredServerPack deletes the server pack a build directory's server.last
// marker names, and the marker. Server packs used to be stored there; they are
// rendered into a local temp cache now and never stored.
func dropStoredServerPack(ctx context.Context, prov modpack.ModpackStorageProvider, dir string) error {
	marker := dir + "server.last"
	rc, _, err := prov.Stream(ctx, marker)
	if errors.Is(err, modpack.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", marker, err)
	}
	prev, _ := io.ReadAll(io.LimitReader(rc, 512))
	rc.Close()
	if p := string(prev); strings.HasPrefix(p, dir+"server-") && strings.HasSuffix(p, ".zip") {
		if err := prov.Delete(ctx, p); err != nil {
			return fmt.Errorf("delete %s: %w", p, err)
		}
	}
	if err := prov.Delete(ctx, marker); err != nil {
		return fmt.Errorf("delete %s: %w", marker, err)
	}
	return nil
}

// serverPacksPurgedKey records that the server packs stored before they became
// ephemeral are gone, so the purge walks every build once, not every boot.
const serverPacksPurgedKey = "modpack_server_packs_purged_at"

// PurgeStoredServerPacks deletes, once, every server pack a build directory
// still holds from when server packs were stored (and handed out by share
// links). The modpack storage cannot list keys, so it walks every account's
// builds and follows each directory's server.last marker.
//
// It never fails the boot: any error is logged and the marker stays unset, so
// the next start tries again. Deleting is idempotent, so a retry only repeats
// work. Without a modpack storage nothing was ever stored, and nothing is
// recorded either, for the same reason PurgeLibraryStorage leaves it unset.
func (s *Manager) PurgeStoredServerPacks(ctx context.Context) {
	if s == nil || s.Store == nil {
		return
	}
	done, err := s.Store.GetSetting(serverPacksPurgedKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("server pack purge: reading %s failed, retrying on the next start: %v", serverPacksPurgedKey, err)
		return
	}
	if done != "" {
		return
	}
	prov, err := s.BuildModpackProvider()
	if err != nil || prov == nil {
		return
	}
	users, err := s.Store.ListUsers()
	if err != nil {
		log.Printf("server pack purge: listing accounts failed, retrying on the next start: %v", err)
		return
	}
	failed := 0
	dirs := 0
	for _, u := range users {
		packs, err := s.Store.ListPacksByOwner(u.ID)
		if err != nil {
			log.Printf("server pack purge: listing packs of %s failed: %v", u.ID, err)
			failed++
			continue
		}
		for i := range packs {
			builds, err := s.Store.ListPackBuilds(packs[i].ID)
			if err != nil {
				log.Printf("server pack purge: listing builds of pack %d failed: %v", packs[i].ID, err)
				failed++
				continue
			}
			for j := range builds {
				for _, dir := range s.PackBuildDirs(&packs[i], &builds[j]) {
					dirs++
					if err := dropStoredServerPack(ctx, prov, dir); err != nil {
						log.Printf("server pack purge: %v", err)
						failed++
					}
				}
			}
		}
	}
	if failed > 0 {
		log.Printf("server pack purge: %d of %d build directories failed, retrying on the next start", failed, dirs)
		return
	}
	if err := s.Store.SetSetting(serverPacksPurgedKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Printf("server pack purge: done but could not record it, it runs again on the next start: %v", err)
		return
	}
	log.Printf("server pack purge: checked %d build directories for stored server packs", dirs)
}
