package handlers

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"dylaris-core/services"
)

type LoaderVersion struct {
	Version     string `json:"version"`
	Stable      bool   `json:"stable"`
	Recommended bool   `json:"recommended,omitempty"`
}

// PreferredLoaderVersion pins the same stable default offered by the picker.
// A migration must not export an empty loader dependency and call it latest.
func (h *VersionHandler) PreferredLoaderVersion(ctx context.Context, software, minecraft string) (string, error) {
	versions, err := h.fetchLoaderVersions(ctx, software, minecraft)
	if err != nil {
		return "", err
	}
	for _, v := range versions {
		if v.Stable {
			return v.Version, nil
		}
	}
	return "", fmt.Errorf("no stable %s loader is available for Minecraft %s", software, minecraft)
}

var loaderGameVersion = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,99}$`)

// GetLoaderVersions returns exact compatible loader builds, not Minecraft lines.
func (h *VersionHandler) GetLoaderVersions(w http.ResponseWriter, r *http.Request) {
	software, mc := r.URL.Query().Get("software"), r.URL.Query().Get("minecraft")
	if !loaderGameVersion.MatchString(mc) || strings.Contains(mc, "..") {
		SendJSONError(w, "A valid Minecraft version is required", http.StatusBadRequest)
		return
	}
	switch software {
	case "fabric", "quilt", "forge", "neoforge":
	default:
		SendJSONError(w, "Unknown mod loader", http.StatusBadRequest)
		return
	}
	key := "dylaris:loader-versions:" + software + ":" + mc
	if h.state.Redis != nil {
		if data, err := h.state.Redis.Get(r.Context(), key).Bytes(); err == nil && len(data) > 0 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
			return
		}
	}
	versions, err := h.fetchLoaderVersions(r.Context(), software, mc)
	if err != nil {
		SendJSONError(w, "Failed to fetch loader versions: "+err.Error(), http.StatusBadGateway)
		return
	}
	data, _ := json.Marshal(map[string]any{"success": true, "software": software, "minecraft": mc, "versions": versions})
	if h.state.Redis != nil {
		h.state.Redis.Set(r.Context(), key, data, 5*time.Minute)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (h *VersionHandler) loaderMetadata(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", services.DylarisUserAgent())
	resp, err := h.loaderHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata provider returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 8<<20 {
		return nil, fmt.Errorf("metadata response exceeds size limit")
	}
	return data, nil
}

func (h *VersionHandler) fetchLoaderVersions(ctx context.Context, software, mc string) ([]LoaderVersion, error) {
	endpoint := "https://meta.fabricmc.net/v2/versions/loader/" + url.PathEscape(mc)
	switch software {
	case "quilt":
		endpoint = "https://meta.quiltmc.org/v3/versions/loader/" + url.PathEscape(mc)
	case "forge":
		endpoint = "https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml"
	case "neoforge":
		endpoint = "https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml"
	}
	data, err := h.loaderMetadata(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	result := []LoaderVersion{}
	if software == "fabric" || software == "quilt" {
		var rows []struct {
			Loader struct {
				Version string `json:"version"`
				Stable  *bool  `json:"stable"`
			} `json:"loader"`
		}
		if err := json.Unmarshal(data, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			v := row.Loader.Version
			if !loaderGameVersion.MatchString(v) || strings.Contains(v, "..") {
				continue
			}
			stable := !strings.ContainsAny(v, "-+")
			if row.Loader.Stable != nil {
				stable = *row.Loader.Stable
			}
			result = append(result, LoaderVersion{Version: v, Stable: stable})
		}
	} else {
		var meta struct {
			XMLName  xml.Name `xml:"metadata"`
			Versions []string `xml:"versioning>versions>version"`
		}
		if err := xml.Unmarshal(data, &meta); err != nil {
			return nil, err
		}
		for _, v := range meta.Versions {
			build := v
			if software == "forge" {
				if !strings.HasPrefix(v, mc+"-") {
					continue
				}
				build = strings.TrimPrefix(v, mc+"-")
			} else if services.NeoForgeMinecraft(v) != mc {
				continue
			}
			if loaderGameVersion.MatchString(build) && !strings.Contains(build, "..") {
				result = append(result, LoaderVersion{Version: build, Stable: !strings.ContainsAny(build, "-+")})
			}
		}
	}
	// Provider order differs, especially Quilt. Numeric comparison keeps 0.29
	// ahead of 0.9 without treating an old first row as the recommended build.
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Stable != result[j].Stable {
			return result[i].Stable
		}
		return newerLoader(result[i].Version, result[j].Version)
	})
	if len(result) > 0 && result[0].Stable {
		result[0].Recommended = true
	}
	return result, nil
}

func newerLoader(a, b string) bool {
	parts := func(v string) []string {
		return strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' })
	}
	ap, bp := parts(a), parts(b)
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if ap[i] == bp[i] {
			continue
		}
		an, ae := strconv.Atoi(ap[i])
		bn, be := strconv.Atoi(bp[i])
		if ae == nil && be == nil {
			return an > bn
		}
		return ap[i] > bp[i]
	}
	return len(ap) > len(bp)
}
