package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"dylaris-core/models"
	"dylaris-core/pkg/crypto"
)

// modpackMirrorPath is the route a node fetches a panel-built pack from. The
// suffix after it is the storage key, so the full URL is
// {core_public_url}/mirror/modpacks/<hmac>/pack.mrpack.
//
// It used to live under /solder/mirror/, because the same route also served the
// Technic launcher. Solder is gone; this is the one job the route still has.
const ModpackMirrorPath = "/mirror/"

// modpackMirrorBase is the absolute base a node is pointed at to download a
// rendered pack. It is Core's own public URL: the only other delivery the old
// route knew was a Solder-only "public bucket" mode, and a pack install was
// never meant to leave Core's hands.
//
// The returned base always ends with a single trailing slash.
func ModpackMirrorBase(getSetting func(string) (string, error)) (string, error) {
	base, _ := getSetting("core_public_url")
	base = strings.TrimSpace(base)
	if base == "" {
		return "", fmt.Errorf("core_public_url is not set (a node needs it to download a pack from this Core)")
	}
	return strings.TrimRight(base, "/") + ModpackMirrorPath, nil
}

// MrpackStorageKey is the derived storage key of a build's rendered .mrpack;
// see packs.PacksHandler.mrpackStorageKey for why it is derived. It lives here
// because the storage cleanup (packBuildDirs) needs it as much as the packs
// handlers do.
func (s *AppState) MrpackStorageKey(pack *models.Pack, build *models.PackBuild) string {
	mac := hmac.New(sha256.New, crypto.DeriveKey(s.ClusterSecret, "mrpack-path"))
	fmt.Fprintf(mac, "%s\x1f%s\x1f%s", pack.OwnerID, pack.InternalSlug, build.VersionString)
	return "modpacks/" + hex.EncodeToString(mac.Sum(nil)) + "/pack.mrpack"
}
