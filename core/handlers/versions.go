package handlers

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/services"
)

// VersionEntry represents a single version with its major group and build string.
type VersionEntry struct {
	Major string `json:"major"` // e.g. "1.21"
	Build string `json:"build"` // e.g. "1.21.4"
}

// SoftwareProvider is the interface for version sources.
type SoftwareProvider interface {
	Name() string
	FetchVersions() ([]VersionEntry, error)
}

// SoftwareInfo is returned by GetSoftwareList with type metadata.
type SoftwareInfo struct {
	Name string `json:"name"`
	Type string `json:"type"` // "game" or "proxy"
}

// ==========================================
// PaperMC Provider (generic for Paper, Velocity, Waterfall)
// ==========================================

type PaperMCProvider struct {
	project string
}

func (p *PaperMCProvider) Name() string { return p.project }

func (p *PaperMCProvider) FetchVersions() ([]VersionEntry, error) {
	// PaperMC retired the v2 API (api.papermc.io/v2 now returns 410 "sunset");
	// the current source is the v3 Fill API. Its `versions` is an object keyed by
	// major line -> build list, not the old flat array. Order is irrelevant here:
	// the panel regroups by major and sorts both columns itself.
	url := fmt.Sprintf("https://fill.papermc.io/v3/projects/%s", p.project)
	resp, err := fetchJSON(url)
	if err != nil {
		return nil, err
	}
	return parsePaperMCVersions(resp, p.project)
}

// parsePaperMCVersions flattens the v3 Fill `versions` object ({major: [builds]})
// into VersionEntry rows, skipping pre-release / RC / snapshot builds (any build
// carrying a "-" suffix) so the picker only offers stable releases, matching the
// old v2 behavior.
func parsePaperMCVersions(resp map[string]interface{}, project string) ([]VersionEntry, error) {
	versions, ok := resp["versions"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected %s API response", project)
	}
	var entries []VersionEntry
	for _, builds := range versions {
		list, ok := builds.([]interface{})
		if !ok {
			continue
		}
		for _, b := range list {
			build, _ := b.(string)
			if build == "" || strings.Contains(build, "-") {
				continue
			}
			entries = append(entries, VersionEntry{
				Major: getMajorVersion(build),
				Build: build,
			})
		}
	}
	return entries, nil
}

// ==========================================
// Vanilla Provider
// ==========================================

type VanillaProvider struct{}

func (v *VanillaProvider) Name() string { return "vanilla" }

func (v *VanillaProvider) FetchVersions() ([]VersionEntry, error) {
	resp, err := fetchJSON("https://launchermeta.mojang.com/mc/game/version_manifest.json")
	if err != nil {
		return nil, err
	}

	rawVersions, ok := resp["versions"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected vanilla API response")
	}

	var entries []VersionEntry
	for _, rv := range rawVersions {
		m, _ := rv.(map[string]interface{})
		if m == nil {
			continue
		}
		if m["type"] != "release" {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		entries = append(entries, VersionEntry{
			Major: getMajorVersion(id),
			Build: id,
		})
	}
	return entries, nil
}

// ==========================================
// BungeeCord Provider (Jenkins CI)
// ==========================================

type BungeeCordProvider struct{}

func (b *BungeeCordProvider) Name() string { return "bungeecord" }

func (b *BungeeCordProvider) FetchVersions() ([]VersionEntry, error) {
	resp, err := fetchJSON("https://ci.md-5.net/job/BungeeCord/api/json?tree=builds[number,result]{0,20}")
	if err != nil {
		return nil, err
	}

	builds, ok := resp["builds"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected BungeeCord CI response")
	}

	var entries []VersionEntry
	for _, rb := range builds {
		m, _ := rb.(map[string]interface{})
		if m == nil {
			continue
		}
		result, _ := m["result"].(string)
		if result != "SUCCESS" {
			continue
		}
		number, _ := m["number"].(float64)
		if number == 0 {
			continue
		}
		build := fmt.Sprintf("#%d", int(number))
		entries = append(entries, VersionEntry{
			Major: "latest",
			Build: build,
		})
	}
	return entries, nil
}

// ==========================================
// Fabric Provider
// ==========================================

type FabricProvider struct{}

func (f *FabricProvider) Name() string { return "fabric" }

func (f *FabricProvider) FetchVersions() ([]VersionEntry, error) {
	// Fabric publishes the supported MC version list separately from
	// loaders. We surface the MC versions; the loader auto-picks latest.
	url := "https://meta.fabricmc.net/v2/versions/game"
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var versions []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err := json.Unmarshal(body, &versions); err != nil {
		return nil, err
	}
	var entries []VersionEntry
	for _, v := range versions {
		if !v.Stable {
			continue
		}
		entries = append(entries, VersionEntry{
			Major: getMajorVersion(v.Version),
			Build: v.Version,
		})
	}
	return entries, nil
}

// ==========================================
// Forge Provider
// ==========================================

type ForgeProvider struct{}

func (f *ForgeProvider) Name() string { return "forge" }

func (f *ForgeProvider) FetchVersions() ([]VersionEntry, error) {
	resp, err := fetchJSON("https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json")
	if err != nil {
		return nil, err
	}
	promos, ok := resp["promos"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected Forge promotions response")
	}
	// Promo keys look like "1.20.1-recommended" or "1.20.1-latest".
	// We emit one entry per MC version, preferring -recommended.
	seen := map[string]bool{}
	var entries []VersionEntry
	for key, val := range promos {
		// Strip suffix
		mc := key
		for _, suffix := range []string{"-recommended", "-latest"} {
			if len(key) > len(suffix) && key[len(key)-len(suffix):] == suffix {
				mc = key[:len(key)-len(suffix)]
				break
			}
		}
		if seen[mc] {
			continue
		}
		seen[mc] = true
		_ = val // build number; the installer fetches it again at install time
		entries = append(entries, VersionEntry{
			Major: getMajorVersion(mc),
			Build: mc,
		})
	}
	return entries, nil
}

// ==========================================
// NeoForge Provider
// ==========================================

type NeoForgeProvider struct{}

func (n *NeoForgeProvider) Name() string { return "neoforge" }

func (n *NeoForgeProvider) FetchVersions() ([]VersionEntry, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var meta struct {
		Versioning struct {
			Versions struct {
				Version []string `xml:"version"`
			} `xml:"versions"`
		} `xml:"versioning"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, err
	}
	var entries []VersionEntry
	all := meta.Versioning.Versions.Version
	for i := len(all) - 1; i >= 0; i-- {
		v := all[i]
		major, ok := neoForgeMajor(v)
		if !ok {
			continue
		}
		entries = append(entries, VersionEntry{
			Major: major,
			Build: v,
		})
	}
	return entries, nil
}

// neoForgeMajor maps a NeoForge build to the Minecraft version it targets, and
// says no to builds the picker must not offer.
//
// "21.1.106" targets MC 1.21.x, so the first number maps to "1.{N}". Since MC
// 26.x the scheme follows Minecraft itself: "26.3.0.89" targets 26.3, which the
// old "1."+first rule called "1.26". Pre-releases (-beta, -alpha..., +snapshot)
// are left out like PaperMC's, and so are the 0.x April-fools builds
// ("0.25w14craftmine.3-beta"), which made a Minecraft "1.0" appear.
func neoForgeMajor(v string) (string, bool) {
	if strings.ContainsAny(v, "-+") {
		return "", false
	}
	parts := splitDot(v)
	if len(parts) < 2 {
		return "", false
	}
	first, err := strconv.Atoi(parts[0])
	if err != nil || first == 0 {
		return "", false
	}
	if first >= 26 {
		return parts[0] + "." + parts[1], true
	}
	return "1." + parts[0], true
}

// neoForgeMinecraft and exactMinecraftVersion live in services so a backup
// import there records the same exact version a setup here does.
func neoForgeMinecraft(loader string) string { return services.NeoForgeMinecraft(loader) }

func exactMinecraftVersion(software, mcVersion, version, loader string) string {
	return services.ExactMinecraftVersion(software, mcVersion, version, loader)
}

// ==========================================
// Provider Registry
// ==========================================

var softwareProviders = map[string]SoftwareProvider{
	"paper":      &PaperMCProvider{project: "paper"},
	"vanilla":    &VanillaProvider{},
	"fabric":     &FabricProvider{},
	"forge":      &ForgeProvider{},
	"neoforge":   &NeoForgeProvider{},
	"velocity":   &PaperMCProvider{project: "velocity"},
	"waterfall":  &PaperMCProvider{project: "waterfall"},
	"bungeecord": &BungeeCordProvider{},
}

// softwareTypes maps provider names to their server type (game or proxy).
var softwareTypes = map[string]string{
	"paper":      "game",
	"vanilla":    "game",
	"fabric":     "game",
	"forge":      "game",
	"neoforge":   "game",
	"velocity":   "proxy",
	"waterfall":  "proxy",
	"bungeecord": "proxy",
}

// ==========================================
// Handler
// ==========================================

type VersionHandler struct {
	state      *AppState
	loaderHTTP *http.Client
}

func NewVersionHandler(state *AppState) *VersionHandler {
	return &VersionHandler{state: state, loaderHTTP: &http.Client{Timeout: 15 * time.Second}}
}

// GetVersions GET /api/versions - the available versions for one server
// software, chosen with ?software=. An unknown value is 400 rather than an
// empty list.
func (h *VersionHandler) GetVersions(w http.ResponseWriter, r *http.Request) {
	software := r.URL.Query().Get("software")
	provider, ok := softwareProviders[software]
	if !ok {
		sendJSONError(w, "Unknown software: "+software, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	cacheKey := fmt.Sprintf("dylaris:versions:%s", software)

	// Try Redis cache first
	if h.state.Redis != nil {
		cached, err := h.state.Redis.Get(ctx, cacheKey).Bytes()
		if err == nil && len(cached) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.Write(cached)
			return
		}
	}

	versions, err := provider.FetchVersions()
	if err != nil {
		sendJSONError(w, "Failed to fetch versions: "+err.Error(), http.StatusBadGateway)
		return
	}

	result := map[string]interface{}{
		"success":  true,
		"software": software,
		"versions": versions,
	}

	data, _ := json.Marshal(result)

	// Cache for 5 minutes
	if h.state.Redis != nil {
		h.state.Redis.Set(ctx, cacheKey, data, 5*time.Minute)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// GetSoftwareList GET /api/versions/software (public)
func (h *VersionHandler) GetSoftwareList(w http.ResponseWriter, r *http.Request) {
	list := make([]SoftwareInfo, 0, len(softwareProviders))
	for name := range softwareProviders {
		stype := softwareTypes[name]
		if stype == "" {
			stype = "game"
		}
		list = append(list, SoftwareInfo{Name: name, Type: stype})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"software": list,
	})
}

// ==========================================
// Helpers
// ==========================================

func getMajorVersion(version string) string {
	parts := splitDot(version)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return version
}

func splitDot(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func fetchJSON(url string) (map[string]interface{}, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}
