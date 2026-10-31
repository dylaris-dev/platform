package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/pkg/crypto"
)

// modpackMirrorPath is the route a node fetches a panel-built pack from. The
// suffix after it is the storage key, so the full URL is
// {core_public_url}/mirror/modpacks/<hmac>/pack.mrpack?exp=..&sig=..
// (SignedModpackMirrorURL).
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

// ModpackMirrorTTL bounds how long a pack download URL handed to a node stays
// valid. Long, because the install command waits in the node's queue while the
// node is offline; the signature is checked when the download STARTS.
const ModpackMirrorTTL = 24 * time.Hour

// modpackMirrorSig binds one pack key to an expiry. Only Core can make one, so a
// URL that leaks opens that one pack until it expires and nothing else.
func modpackMirrorSig(secret string, exp int64, key string) string {
	mac := hmac.New(sha256.New, crypto.DeriveKey(secret, "modpack-mirror"))
	fmt.Fprintf(mac, "%d\x1f%s", exp, key)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignedModpackMirrorURL is where a node downloads a rendered pack from. The
// signature rides in the query so the host and the path stay what an older
// node checks (Core's host, a key ending in pack.mrpack).
func SignedModpackMirrorURL(base, secret, key string, now time.Time) string {
	exp := now.Add(ModpackMirrorTTL).Unix()
	return base + key + "?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + modpackMirrorSig(secret, exp, key)
}

// ValidModpackMirrorSig reports whether sig signs key until expRaw, and expRaw
// has not passed.
func ValidModpackMirrorSig(secret, key, expRaw, sig string, now time.Time) bool {
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil || secret == "" || now.Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(modpackMirrorSig(secret, exp, key)))
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
