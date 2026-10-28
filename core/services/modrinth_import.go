package services

// Importing an existing Modrinth modpack: the projects a user is a member of,
// each project's versions, and everything inside one version's .mrpack.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// ModrinthProjectSummary is one row of the import list.
type ModrinthProjectSummary struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	ProjectType string   `json:"project_type"`
	Status      string   `json:"status"`
	IconURL     *string  `json:"icon_url"`
	Versions    []string `json:"versions"`
	Team        string   `json:"team"`
}

// ModrinthProjectVersion is a version as the import needs it.
type ModrinthProjectVersion struct {
	ID            string         `json:"id"`
	ProjectID     string         `json:"project_id"`
	Name          string         `json:"name"`
	VersionNumber string         `json:"version_number"`
	VersionType   string         `json:"version_type"`
	Changelog     string         `json:"changelog"`
	DatePublished time.Time      `json:"date_published"`
	Status        string         `json:"status"`
	GameVersions  []string       `json:"game_versions"`
	Loaders       []string       `json:"loaders"`
	Files         []ModrinthFile `json:"files"`
}

// PrimaryMrpack is the version's .mrpack file, or nil.
func (v *ModrinthProjectVersion) PrimaryMrpack() *ModrinthFile {
	var fallback *ModrinthFile
	for i := range v.Files {
		f := &v.Files[i]
		if !strings.HasSuffix(strings.ToLower(f.Filename), ".mrpack") {
			continue
		}
		if f.Primary {
			return f
		}
		if fallback == nil {
			fallback = f
		}
	}
	return fallback
}

// CurrentUser is the account the token belongs to.
func (c *ModrinthClient) CurrentUser(ctx context.Context) (id, username string, err error) {
	var u struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := c.doJSON(ctx, http.MethodGet, ModrinthAPIBase+"/user", nil, &u); err != nil {
		return "", "", err
	}
	return u.ID, u.Username, nil
}

// UserProjects lists every project the user is a member of, drafts included
// when the token is that user's own.
func (c *ModrinthClient) UserProjects(ctx context.Context, userID string) ([]ModrinthProjectSummary, error) {
	var out []ModrinthProjectSummary
	if err := c.doJSON(ctx, http.MethodGet, ModrinthAPIBase+"/user/"+url.PathEscape(userID)+"/projects", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ProjectVersions lists a project's versions, newest first as Modrinth sends them.
func (c *ModrinthClient) ProjectVersions(ctx context.Context, projectID string) ([]ModrinthProjectVersion, error) {
	var out []ModrinthProjectVersion
	if err := c.doJSON(ctx, http.MethodGet, ModrinthAPIBase+"/project/"+url.PathEscape(projectID)+"/version", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FetchModrinthVersionsBulk resolves many version ids in few requests
// (GET /v2/versions?ids=[...]), paced like the other user-triggered lookups.
func FetchModrinthVersionsBulk(ctx context.Context, ids []string) (map[string]*ModrinthVersion, error) {
	out := map[string]*ModrinthVersion{}
	const chunk = 100
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		raw, err := json.Marshal(ids[start:end])
		if err != nil {
			return nil, err
		}
		var page []ModrinthVersion
		if err := modrinthGetPaced(ctx, "/versions?ids="+url.QueryEscape(string(raw)), &page); err != nil {
			return nil, err
		}
		for i := range page {
			out[page[i].ID] = &page[i]
		}
	}
	return out, nil
}

// MrpackAllowedHosts mirrors Modrinth's approved download hosts, the same set
// the node accepts when it installs a pack (node/installer_modpack.go). A pack
// whose manifest points anywhere else is not fetched by Core either.
var MrpackAllowedHosts = map[string]bool{
	"cdn.modrinth.com":          true,
	"github.com":                true,
	"raw.githubusercontent.com": true,
	"gitlab.com":                true,
	"maven.fabricmc.net":        true,
	"maven.minecraftforge.net":  true,
	// GitHub serves release assets through redirects to these.
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
}

var importHTTP = &http.Client{
	Timeout: 5 * time.Minute,
	// Every hop must stay on an allowed host: a redirect is just another URL a
	// pack author chose.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" || !MrpackAllowedHosts[strings.ToLower(req.URL.Hostname())] {
			return fmt.Errorf("redirect to a host outside the allowlist: %s", req.URL.Hostname())
		}
		return nil
	},
}

// DownloadAllowed fetches an https URL on an allowed host, at most max bytes.
func DownloadAllowed(ctx context.Context, rawURL string, max int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || !MrpackAllowedHosts[strings.ToLower(u.Hostname())] {
		return nil, fmt.Errorf("download host not allowed: %s", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", DylarisUserAgent())
	resp, err := importHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: status %d", u.Hostname(), resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("download exceeds %d bytes", max)
	}
	return data, nil
}

// MrpackFile is one manifest entry, with everything the import needs.
type MrpackFile struct {
	Path      string
	SHA1      string
	Side      string
	Downloads []string
	Size      int64
	// Set when the first download is a Modrinth CDN URL.
	ModrinthProjectID string
	ModrinthVersionID string
}

// MrpackOverride is one file from overrides/, client-overrides/ or
// server-overrides/, with the side that folder means.
type MrpackOverride struct {
	Path string // relative to the Minecraft instance
	Side string
	Data []byte
}

// MrpackFull is a parsed .mrpack.
type MrpackFull struct {
	Name         string
	Dependencies map[string]string // "minecraft", "forge", "neoforge", "fabric-loader", "quilt-loader"
	Files        []MrpackFile
	Overrides    []MrpackOverride
}

// Limits for one imported pack, so a crafted archive cannot exhaust Core.
const (
	MaxImportOverrideFiles = 4000
	MaxImportOverrideFile  = 64 << 20  // one override file
	MaxImportOverrideTotal = 512 << 20 // all override files together
)

// ParseMrpackFull reads the manifest and every override file of a .mrpack.
func ParseMrpackFull(data []byte) (*MrpackFull, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open mrpack: %w", err)
	}
	out := &MrpackFull{}
	var manifestFound bool
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Name == "modrinth.index.json" {
			if f.UncompressedSize64 > uint64(maxMrpackIndexJSONBytes) {
				return nil, fmt.Errorf("modrinth.index.json exceeds the %d byte cap", maxMrpackIndexJSONBytes)
			}
			var m struct {
				Name  string `json:"name"`
				Files []struct {
					Path      string            `json:"path"`
					Hashes    map[string]string `json:"hashes"`
					Env       map[string]string `json:"env"`
					Downloads []string          `json:"downloads"`
					FileSize  int64             `json:"fileSize"`
				} `json:"files"`
				Dependencies map[string]string `json:"dependencies"`
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			err = json.NewDecoder(io.LimitReader(rc, int64(maxMrpackIndexJSONBytes)+1)).Decode(&m)
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("parse modrinth.index.json: %w", err)
			}
			manifestFound = true
			out.Name = m.Name
			out.Dependencies = m.Dependencies
			for _, mf := range m.Files {
				file := MrpackFile{Path: mf.Path, SHA1: mf.Hashes["sha1"], Side: sideFromEnv(mf.Env), Downloads: mf.Downloads, Size: mf.FileSize}
				if len(mf.Downloads) > 0 {
					file.ModrinthProjectID, file.ModrinthVersionID, _ = parseModrinthCDNURL(mf.Downloads[0])
				}
				out.Files = append(out.Files, file)
			}
			continue
		}
		side := ""
		var rel string
		for prefix, s := range map[string]string{"overrides/": "both", "client-overrides/": "client", "server-overrides/": "server"} {
			if strings.HasPrefix(f.Name, prefix) {
				side, rel = s, strings.TrimPrefix(f.Name, prefix)
			}
		}
		if side == "" || rel == "" {
			continue
		}
		clean := path.Clean(rel)
		if clean != rel || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, "\\") {
			return nil, fmt.Errorf("unsafe path in pack: %s", f.Name)
		}
		if len(out.Overrides) >= MaxImportOverrideFiles {
			return nil, fmt.Errorf("pack has more than %d override files", MaxImportOverrideFiles)
		}
		if f.UncompressedSize64 > MaxImportOverrideFile {
			return nil, fmt.Errorf("override %s is larger than %d bytes", rel, MaxImportOverrideFile)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, MaxImportOverrideFile+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(b)) > MaxImportOverrideFile {
			return nil, fmt.Errorf("override %s is larger than %d bytes", rel, MaxImportOverrideFile)
		}
		total += int64(len(b))
		if total > MaxImportOverrideTotal {
			return nil, fmt.Errorf("pack overrides exceed %d bytes", MaxImportOverrideTotal)
		}
		out.Overrides = append(out.Overrides, MrpackOverride{Path: rel, Side: side, Data: b})
	}
	if !manifestFound {
		return nil, errors.New("not a Modrinth pack: modrinth.index.json missing")
	}
	return out, nil
}

// MrpackLoader turns a manifest's dependencies into the build's loader name
// and loader version ("" / "" for vanilla).
func MrpackLoader(deps map[string]string) (loader, version string) {
	for key, name := range map[string]string{"neoforge": "neoforge", "forge": "forge", "fabric-loader": "fabric", "quilt-loader": "quilt"} {
		if v, ok := deps[key]; ok {
			return name, v
		}
	}
	return "", ""
}
