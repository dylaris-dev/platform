package beam

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// defaultBeamManifestURL is the stable-channel signed update manifest on GitHub
// Releases. "latest/download" always resolves to the newest NON-prerelease
// release's fixed-name assets. Shared by the download-URL resolver and the auto
// min-version follower; overridable per-deploy via the beam.release_manifest
// setting. The compiled-in fallback (used when that setting is empty) is
// BEAM_MANIFEST_URL so a fork points at its OWN releases repo without a rebuild;
// it defaults to the upstream repo. Read once at startup. Public once the repo is
// public (owner go-live step).
var defaultBeamManifestURL = func() string {
	if v := strings.TrimSpace(os.Getenv("BEAM_MANIFEST_URL")); v != "" {
		return v
	}
	return "https://github.com/Bartis-Dev/dylaris-platform/releases/latest/download/latest.json"
}()

// beamUpdatePublicKeyB64 is the base64 (std) Ed25519 PUBLIC key that signs the
// beam update manifest (latest.json). It MUST stay byte-identical to the app's
// copy in platform/beam/app/update_pubkey.go: Core follows the signed manifest's
// minVersion in "auto" mode and the app verifies its own self-update, so both
// ends must root trust in the same key. Public keys are safe to commit and to
// duplicate across the two modules (beam/app is a separate go.work member and
// cannot be imported here).
//
// This is the REAL signing key; the matching private seed lives only in the CI
// secret BEAM_UPDATE_PRIVKEY (generated once via `go run ./cmd/beam-release
// keygen` from platform/beam/app). If this value is emptied or drifts from the
// app's copy, no release signature matches, so auto min-version resolves to ""
// (gate off). Fail-OPEN on the floor is deliberate: an unverifiable manifest must
// never be trusted to raise the floor and lock every client out.
const beamUpdatePublicKeyB64 = "5WS1g2Ushib55CKq7duxHJlcJKIwD7L3wYdA6coTiA4="

// beamMinAutoTTL bounds how often Core re-fetches the signed manifest for the
// auto min-version floor. GetBeamTicket runs on every connect, so an uncached
// fetch per request would add a GitHub round-trip to the hot connect path.
const beamMinAutoTTL = 5 * time.Minute

// errBeamManifestUnverified is the single fail-closed error for any manifest
// signature failure. Callers translate it to "no auto floor" ("").
var errBeamManifestUnverified = errors.New("beam: manifest signature verification failed")

// errBeamPlatformNotInManifest separates "we do not build for this platform" from
// every other reason a download cannot start.
//
// Collapsing the two told a macOS visitor to the public download link, verbatim,
// to "check the signed release manifest, or set beam.download_link" - operator
// vocabulary on an unauthenticated route, for the ordinary and permanent case
// that there is no macOS build. Worse in the other direction: while GitHub is
// unreachable EVERY platform looks unbuilt, so a Windows user would be told
// something false and the real outage would be hidden behind it.
var errBeamPlatformNotInManifest = errors.New("beam: no build for this platform")

// beamManifest is Core's read view of the signed latest.json. Only the fields
// Core needs are modeled; the per-binary sigs are ignored here (the app consumes
// those). Read ONLY after the signature verifies.
//
// Platforms joined this struct when the download endpoint stopped decoding the
// same document on its own: it used to unmarshal latest.json without ever
// fetching latest.json.sig, so the URL it streamed an executable from came out
// of a document nothing had authenticated.
type beamManifest struct {
	Version    string `json:"version"`
	MinVersion string `json:"minVersion"`
	Platforms  map[string]struct {
		URL string `json:"url"`
		// Sha256 is the hex digest over the binary itself, produced by
		// cmd/beam-release and covered by the manifest signature. The app's
		// updater has always checked it; Core now does too, so that an operator
		// mirror (beam.download_link) can change WHERE the bytes come from
		// without changing WHAT they are allowed to be.
		Sha256 string `json:"sha256"`
	} `json:"platforms"`
}

// fetchVerifiedBeamPlatformURL returns the download URL for one platform slug
// from the SIGNED manifest, or an error if the signature does not verify.
//
// Fail-CLOSED, unlike fetchVerifiedBeamMinVersion next to it, and the asymmetry
// is deliberate: an unverifiable manifest must not raise the min-version floor
// (that would lock every client out), but it equally must not be the source of
// a URL Core hands an executable down from. No manifest, no download.
func fetchVerifiedBeamPlatformURL(ctx context.Context, manifestURL, pubB64, platform string) (string, error) {
	u, _, err := fetchVerifiedBeamPlatformArtifact(ctx, manifestURL, pubB64, platform)
	return u, err
}

// fetchVerifiedBeamPlatformArtifact returns the URL AND the expected hex sha256
// for one platform, both out of the signature-verified manifest.
//
// The digest is what lets beam.download_link stay useful without being a hole:
// the manifest decides what the bytes must be, the setting only decides where
// they are fetched from.
func fetchVerifiedBeamPlatformArtifact(ctx context.Context, manifestURL, pubB64, platform string) (url, sha256Hex string, err error) {
	body, err := httpGetBeamManifestBytes(ctx, manifestURL)
	if err != nil {
		return "", "", err
	}
	sig, err := httpGetBeamManifestBytes(ctx, manifestURL+".sig")
	if err != nil {
		return "", "", err
	}
	m, err := verifyBeamManifest(pubB64, body, sig)
	if err != nil {
		return "", "", err
	}
	p, ok := m.Platforms[platform]
	if !ok || strings.TrimSpace(p.URL) == "" {
		return "", "", fmt.Errorf("%w: %s", errBeamPlatformNotInManifest, platform)
	}
	return strings.TrimSpace(p.URL), strings.ToLower(strings.TrimSpace(p.Sha256)), nil
}

// verifyBeamManifest verifies sigB64 (base64-std, over the exact body bytes)
// against pubB64, then unmarshals. FAIL-CLOSED on every bad-input path: an
// unparseable/placeholder/wrong-length key, a bad signature encoding, an empty
// signature, or a failed verify all return errBeamManifestUnverified and never a
// partial manifest. Mirrors the app's parseVerifiedManifest so both ends agree.
func verifyBeamManifest(pubB64 string, body, sigB64 []byte) (*beamManifest, error) {
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pubB64))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errBeamManifestUnverified
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil || len(sig) == 0 {
		return nil, errBeamManifestUnverified
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), body, sig) {
		return nil, errBeamManifestUnverified
	}
	var m beamManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// fetchVerifiedBeamMinVersion downloads latest.json + latest.json.sig from
// manifestURL, verifies the signature with pubB64, and returns the manifest's
// embedded minVersion (may be ""). Any fetch or verify error is returned so the
// caller treats it as "no auto floor".
func fetchVerifiedBeamMinVersion(ctx context.Context, manifestURL, pubB64 string) (string, error) {
	body, err := httpGetBeamManifestBytes(ctx, manifestURL)
	if err != nil {
		return "", err
	}
	sig, err := httpGetBeamManifestBytes(ctx, manifestURL+".sig")
	if err != nil {
		return "", err
	}
	m, err := verifyBeamManifest(pubB64, body, sig)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(m.MinVersion), nil
}

// httpGetBeamManifestBytes GETs a small manifest asset with a tight timeout and a
// 1 MiB read cap (a manifest is a few hundred bytes; the cap guards against a
// hostile/oversized response on the connect hot path).
func httpGetBeamManifestBytes(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// Same SSRF-guarded dial the binary fetch uses: beam.release_manifest is a
	// settings-table override, and this fetch is on the connect hot path.
	resp, err := (&http.Client{
		Timeout:   8 * time.Second,
		Transport: &http.Transport{DialContext: beamDialContext},
	}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest fetch %s: HTTP %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// effectiveMinVersion returns the force-update floor Core advertises
// (GetBeamConfig) and enforces (GetBeamTicket): the signed manifest's minVersion,
// fetched+verified at most once per beamMinAutoTTL and cached.
//
// There used to be a second, manual mode where an admin typed the floor into
// Settings -> Beam. It is gone: the floor is a property of the release, the
// release manifest already carries it under a signature both ends verify, and a
// hand-typed value could only ever disagree with the binary actually being
// shipped - in the direction that locks clients out.
//
// A fetch or verify failure yields "" (gate off), so a transient GitHub error
// can never lock every client out either.
func (h *BeamHandler) effectiveMinVersion(ctx context.Context) string {
	return h.cachedAutoMinVersion(ctx, func(key string) string {
		v, _ := h.state.Store.GetSetting(key)
		return v
	})
}

// cachedAutoMinVersion returns the verified-manifest floor, refreshing at most
// once per beamMinAutoTTL. The outcome is cached either way (min="" on error) so
// a failing fetch does not re-hit GitHub on every connect; the floor simply stays
// off until the next TTL window resolves a verifiable manifest. Guarded so
// concurrent connects don't stampede the fetch.
func (h *BeamHandler) cachedAutoMinVersion(ctx context.Context, getSetting func(string) string) string {
	h.minCacheMu.Lock()
	defer h.minCacheMu.Unlock()
	if !h.minCacheAt.IsZero() && time.Since(h.minCacheAt) < beamMinAutoTTL {
		return h.minCacheVal
	}
	url := strings.TrimSpace(getSetting("beam.release_manifest"))
	if url == "" {
		url = defaultBeamManifestURL
	}
	min, err := fetchVerifiedBeamMinVersion(ctx, url, beamUpdatePublicKeyB64)
	if err != nil {
		min = ""
	}
	h.minCacheVal = min
	h.minCacheAt = time.Now()
	return min
}
