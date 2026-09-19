package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// The panel already has a Modrinth *proxy* in handlers/modrinth.go, but that one
// writes straight to an http.ResponseWriter (cache passthrough). Content
// ingestion needs parsed version objects to resolve dependencies and auto-link
// uploads, so this file carries its own small parse-and-return client.

// ModrinthFile is one downloadable artifact of a Modrinth version.
type ModrinthFile struct {
	Filename string            `json:"filename"`
	URL      string            `json:"url"`
	Primary  bool              `json:"primary"`
	Size     int64             `json:"size"`
	Hashes   map[string]string `json:"hashes"`
}

// modrinthVersion is the subset of a Modrinth /version object we need.
type modrinthVersion struct {
	ID           string         `json:"id"`
	ProjectID    string         `json:"project_id"`
	Name         string         `json:"name"`
	VersionNum   string         `json:"version_number"`
	GameVersions []string       `json:"game_versions"`
	Loaders      []string       `json:"loaders"`
	DatePub      string         `json:"date_published"`
	Files        []ModrinthFile `json:"files"`
	Dependencies []struct {
		VersionID      string `json:"version_id"`
		ProjectID      string `json:"project_id"`
		DependencyType string `json:"dependency_type"`
	} `json:"dependencies"`
}

// ModrinthVersion is the exported alias handlers use.
type ModrinthVersion = modrinthVersion

const modrinthAPI = "https://api.modrinth.com/v2"

var modrinthHTTP = &http.Client{Timeout: 15 * time.Second}

// ModrinthHTTPError is a non-200 answer from Modrinth, with the status kept.
//
// It used to be a formatted string, which erased the one distinction that
// matters to anything showing the result to a person: 404 means "Modrinth does
// not know this", and everything else means "Modrinth did not answer". Both
// arrived as a nil and were reported as the former.
type ModrinthHTTPError struct {
	Path   string
	Status int
	Body   string
}

func (e *ModrinthHTTPError) Error() string {
	return fmt.Sprintf("modrinth %s: %d %s", e.Path, e.Status, e.Body)
}

// ModrinthNotFound reports whether err is Modrinth saying it does not know the
// thing that was asked for, as opposed to not answering at all.
func ModrinthNotFound(err error) bool {
	var httpErr *ModrinthHTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound
}

func modrinthGet(path string, out interface{}) error {
	req, _ := http.NewRequest("GET", modrinthAPI+path, nil)
	req.Header.Set("User-Agent", DylarisUserAgent())
	res, err := modrinthHTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		return &ModrinthHTTPError{Path: path, Status: res.StatusCode, Body: string(b)}
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// FetchModrinthVersion returns a single version by id.
func FetchModrinthVersion(versionID string) (*modrinthVersion, error) {
	var v modrinthVersion
	if err := modrinthGet("/version/"+url.PathEscape(versionID), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ModrinthByHash returns the version matching an sha1 hash, or nil.
func ModrinthByHash(sha1hex string) *modrinthVersion {
	v, _ := ModrinthByHashErr(sha1hex)
	return v
}

// ModrinthByHashErr is ModrinthByHash with the reason kept.
//
// The nil-on-anything version above is right for the two import paths, where a
// miss just leaves a file unlinked. It is wrong wherever the answer is shown to
// someone: "no Modrinth version has this hash" and "Modrinth did not answer"
// look identical from a nil, so an outage told every operator their jars were
// not on Modrinth - advice they might act on by deleting them.
func ModrinthByHashErr(sha1hex string) (*modrinthVersion, error) {
	var v modrinthVersion
	if err := modrinthGet("/version_file/"+url.PathEscape(sha1hex)+"?algorithm=sha1", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// CheckLatestVersions batch-queries Modrinth for the latest version matching the
// given loaders + game versions for each file hash. loaders and game_versions are
// REQUIRED by the endpoint. The result map is keyed by the ORIGINAL input hash.
// Unauthenticated (public read).
func CheckLatestVersions(hashes []string, algorithm string, loaders, gameVersions []string) (map[string]ModrinthVersion, error) {
	payload := map[string]interface{}{
		"hashes":        hashes,
		"algorithm":     algorithm,
		"loaders":       loaders,
		"game_versions": gameVersions,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("POST", modrinthAPI+"/version_files/update", bytes.NewReader(buf))
	req.Header.Set("User-Agent", DylarisUserAgent())
	req.Header.Set("Content-Type", "application/json")
	res, err := modrinthHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("modrinth version_files/update: %d %s", res.StatusCode, string(b))
	}
	out := map[string]ModrinthVersion{}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// ProjectSlugOrID returns a slug source (Modrinth version carries project_id;
// use it as the catalog slug basis when no better name exists).
func (v *modrinthVersion) ProjectSlugOrID() string {
	if v.ProjectID != "" {
		return v.ProjectID
	}
	return v.Name
}

// PrimaryFile returns the primary file (or the first) of the version. When the
// version has no files it returns a zero ModrinthFile with an empty Hashes map
// so callers can index Hashes without a nil check.
func (v *modrinthVersion) PrimaryFile() ModrinthFile {
	for _, f := range v.Files {
		if f.Primary {
			return f
		}
	}
	if len(v.Files) > 0 {
		return v.Files[0]
	}
	return ModrinthFile{Hashes: map[string]string{}}
}

// LatestProjectVersionFor picks the newest version of a project compatible with
// mc+loader. Exported for the migration path, which needs a fallback for
// content whose stored hash Modrinth cannot place.
func LatestProjectVersionFor(projectID, mc, loader string) (*modrinthVersion, error) {
	q := url.Values{}
	if loader != "" {
		q.Set("loaders", fmt.Sprintf("[%q]", loader))
	}
	if mc != "" {
		q.Set("game_versions", fmt.Sprintf("[%q]", mc))
	}
	var list []modrinthVersion
	if err := modrinthGet("/project/"+url.PathEscape(projectID)+"/version?"+q.Encode(), &list); err != nil {
		return nil, err
	}
	// The list order is not contractual; pick the newest by date_published.
	var best *modrinthVersion
	for i := range list {
		if best == nil || list[i].DatePub > best.DatePub {
			best = &list[i]
		}
	}
	return best, nil
}

// ResolvedDep is a required dependency the caller should add to the build.
type ResolvedDep struct {
	Version *modrinthVersion
}

// ResolveModrinthDependencies walks required dependencies of the given version
// (recursively), deduping by project id, and returns the versions to add. The
// caller passes the build's mc + loader to resolve version_id-less deps.
func ResolveModrinthDependencies(root *modrinthVersion, mc, loader string, alreadyHave map[string]bool) ([]ResolvedDep, error) {
	out := []ResolvedDep{}
	seen := map[string]bool{}
	var walk func(v *modrinthVersion) error
	walk = func(v *modrinthVersion) error {
		for _, d := range v.Dependencies {
			if d.DependencyType != "required" || d.ProjectID == "" {
				continue
			}
			if alreadyHave[d.ProjectID] || seen[d.ProjectID] {
				continue
			}
			seen[d.ProjectID] = true
			var dv *modrinthVersion
			var err error
			if d.VersionID != "" {
				dv, err = FetchModrinthVersion(d.VersionID)
			} else {
				dv, err = LatestProjectVersionFor(d.ProjectID, mc, loader)
			}
			if err != nil || dv == nil {
				continue // skip unresolvable deps rather than fail the whole add
			}
			out = append(out, ResolvedDep{Version: dv})
			if err := walk(dv); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return out, nil
}
