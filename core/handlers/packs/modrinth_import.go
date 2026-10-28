package packs

// Importing existing Modrinth modpacks. The import list shows every modpack
// project the caller is a member of on Modrinth (their own token decides
// that); importing one creates a pack bound to the project, and each Modrinth
// version becomes a frozen build marked "imported from Modrinth" with its full
// content: Modrinth files as linked entries, other allowed downloads and every
// override file as uploads. The version's own .mrpack is kept as the build's
// artifact, so downloads and server installs use exactly what Modrinth has.

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage/modpack"

	"github.com/gorilla/mux"
)

const (
	maxImportMrpackBytes = 200 << 20 // as for installs (node/installer_modpack.go)
	maxImportFileBytes   = 256 << 20 // one non-Modrinth file a manifest downloads
)

type importProjectRow struct {
	services.ModrinthProjectSummary
	Hidden bool `json:"hidden"`
	PackID int  `json:"packId,omitempty"`
}

// callerModrinth loads the CALLER's own token: the import list is "my Modrinth".
func (h *PacksHandler) callerModrinth(w http.ResponseWriter, r *http.Request) (string, *services.ModrinthClient, bool) {
	userID, _ := r.Context().Value("userID").(string)
	if h.pat == nil {
		handlers.SendJSONError(w, "Modrinth is not available", http.StatusServiceUnavailable)
		return "", nil, false
	}
	pat, _, err := h.pat.LoadPAT(userID)
	if err != nil {
		handlers.SendJSONError(w, "Connect your Modrinth token first (Account, Modrinth).", http.StatusPreconditionRequired)
		return "", nil, false
	}
	return userID, services.NewModrinthClient(pat), true
}

// memberModpacks are the modpack projects the token's user is a member of.
func memberModpacks(ctx context.Context, mc *services.ModrinthClient) ([]services.ModrinthProjectSummary, error) {
	modrinthUser, _, err := mc.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	all, err := mc.UserProjects(ctx, modrinthUser)
	if err != nil {
		return nil, err
	}
	out := []services.ModrinthProjectSummary{}
	seen := map[string]bool{}
	for _, p := range all {
		if p.ProjectType == "modpack" {
			out = append(out, p)
			seen[p.ID] = true
		}
	}
	// Organization projects are not in /user/{id}/projects; their members
	// reach them through the organization's default permissions.
	if orgProjects, err := mc.UserOrganizationProjects(ctx, modrinthUser); err == nil {
		for _, p := range orgProjects {
			if !seen[p.ID] {
				out = append(out, p)
				seen[p.ID] = true
			}
		}
	}
	return out, nil
}

// ListModrinthProjects GET /api/me/modrinth/projects
func (h *PacksHandler) ListModrinthProjects(w http.ResponseWriter, r *http.Request) {
	userID, mc, ok := h.callerModrinth(w, r)
	if !ok {
		return
	}
	projects, err := memberModpacks(r.Context(), mc)
	if err != nil {
		handlers.SendJSONError(w, "Could not read your Modrinth projects: "+err.Error(), http.StatusBadGateway)
		return
	}
	hidden, err := h.state.Store.HiddenModrinthProjects(userID)
	if err != nil {
		handlers.SendJSONError(w, "Could not read hidden projects", http.StatusInternalServerError)
		return
	}
	hiddenSet := map[string]bool{}
	for _, id := range hidden {
		hiddenSet[id] = true
	}
	rows := make([]importProjectRow, 0, len(projects))
	for _, p := range projects {
		row := importProjectRow{ModrinthProjectSummary: p, Hidden: hiddenSet[p.ID]}
		if pack, err := h.state.Store.GetPackByModrinthProject(p.ID); err == nil && pack != nil {
			row.PackID = pack.ID
		}
		rows = append(rows, row)
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "projects": rows})
}

// SetModrinthProjectHidden PUT /api/me/modrinth/projects/{projectId}/hidden
func (h *PacksHandler) SetModrinthProjectHidden(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	projectID := mux.Vars(r)["projectId"]
	var req struct {
		Hidden bool `json:"hidden"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || projectID == "" || len(projectID) > 64 {
		handlers.SendJSONError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetModrinthProjectHidden(userID, projectID, req.Hidden); err != nil {
		handlers.SendJSONError(w, "Could not save", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// ImportModrinthProject POST /api/me/modrinth/projects/{projectId}/import
// creates (or finds) the caller's pack for a project they are a member of.
// The versions are imported one by one afterwards, so the panel can show
// progress and one bad version does not sink the rest.
func (h *PacksHandler) ImportModrinthProject(w http.ResponseWriter, r *http.Request) {
	userID, mc, ok := h.callerModrinth(w, r)
	if !ok {
		return
	}
	projectID := mux.Vars(r)["projectId"]
	projects, err := memberModpacks(r.Context(), mc)
	if err != nil {
		handlers.SendJSONError(w, "Could not read your Modrinth projects: "+err.Error(), http.StatusBadGateway)
		return
	}
	var project *services.ModrinthProjectSummary
	for i := range projects {
		if projects[i].ID == projectID {
			project = &projects[i]
		}
	}
	if project == nil {
		handlers.SendJSONError(w, "That Modrinth modpack is not one of yours.", http.StatusForbidden)
		return
	}
	// One Modrinth project is one pack: a teammate who imported it first
	// already made it, and the team decides who reaches it.
	if existing, err := h.state.Store.GetPackByModrinthProject(projectID); err == nil && existing != nil {
		h.membershipFor(r.Context(), userID, projectID)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "pack": existing, "existing": true})
		return
	}
	packs, _ := h.state.Store.ListPacksByOwner(userID)
	slug := project.Slug
	taken := map[string]bool{}
	for _, p := range packs {
		taken[p.InternalSlug] = true
	}
	for n := 2; taken[slug] || !packSlugRe.MatchString(slug); n++ {
		slug = handlers.Slugify(project.Slug) + "-" + strconv.Itoa(n)
		if n > 50 {
			handlers.SendJSONError(w, "Could not find a free slug for this pack", http.StatusConflict)
			return
		}
	}
	p := &models.Pack{
		OwnerID:             userID,
		InternalName:        strings.TrimSpace(project.Title),
		InternalSlug:        slug,
		Summary:             strings.TrimSpace(project.Description),
		ModrinthProjectID:   project.ID,
		ModrinthProjectName: project.Slug,
		ModrinthVisibility:  "unlisted",
	}
	id, err := h.state.Store.CreatePack(p)
	if err != nil {
		handlers.SendJSONError(w, "Failed to create pack", http.StatusInternalServerError)
		return
	}
	p.ID = id
	h.membershipFor(r.Context(), userID, project.ID)
	h.audit(r, userID, id, "pack.imported", map[string]any{"modrinthProject": project.ID})
	h.state.Events.Publish(r.Context(), "packs.changed", map[string]interface{}{"ownerId": userID})
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "pack": p})
}

type importVersionRow struct {
	ID            string   `json:"id"`
	VersionNumber string   `json:"versionNumber"`
	VersionType   string   `json:"versionType"`
	GameVersions  []string `json:"gameVersions"`
	Loaders       []string `json:"loaders"`
	DatePublished string   `json:"datePublished"`
	BuildID       int      `json:"buildId,omitempty"`
	// New: published after the newest version the pack has, so it is synced
	// automatically (an older missing version is only imported by hand).
	New bool `json:"new,omitempty"`
}

// ListModrinthVersions GET /api/packs/{id}/modrinth/versions - the project's
// versions and which of them this pack already has (for import and sync).
func (h *PacksHandler) ListModrinthVersions(w http.ResponseWriter, r *http.Request) {
	pack, mc, ok := h.modrinthPageClient(w, r, packRead)
	if !ok {
		return
	}
	versions, err := mc.ProjectVersions(r.Context(), pack.ModrinthProjectID)
	if err != nil {
		handlers.SendJSONError(w, "Could not read the Modrinth versions: "+err.Error(), http.StatusBadGateway)
		return
	}
	builds, _ := h.state.Store.ListPackBuilds(pack.ID)
	have := map[string]int{}
	for _, b := range builds {
		if b.ModrinthVersionID != "" {
			have[b.ModrinthVersionID] = b.ID
		}
	}
	fresh := map[string]bool{}
	for _, v := range newModrinthVersions(versions, builds) {
		fresh[v.ID] = true
	}
	rows := make([]importVersionRow, 0, len(versions))
	for _, v := range versions {
		rows = append(rows, importVersionRow{ID: v.ID, VersionNumber: v.VersionNumber, VersionType: v.VersionType, GameVersions: v.GameVersions, Loaders: v.Loaders, DatePublished: v.DatePublished.UTC().Format("2006-01-02T15:04:05Z"), BuildID: have[v.ID], New: fresh[v.ID]})
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "versions": rows})
}

// ImportModrinthVersion POST /api/packs/{id}/modrinth/versions/{versionId}/import
func (h *PacksHandler) ImportModrinthVersion(w http.ResponseWriter, r *http.Request) {
	pack, mc, ok := h.modrinthPageClient(w, r, packEdit)
	if !ok {
		return
	}
	versionID := mux.Vars(r)["versionId"]
	versions, err := mc.ProjectVersions(r.Context(), pack.ModrinthProjectID)
	if err != nil {
		handlers.SendJSONError(w, "Could not read the Modrinth versions: "+err.Error(), http.StatusBadGateway)
		return
	}
	var version *services.ModrinthProjectVersion
	for i := range versions {
		if versions[i].ID == versionID {
			version = &versions[i]
		}
	}
	if version == nil {
		handlers.SendJSONError(w, "That version does not belong to this pack's Modrinth project.", http.StatusNotFound)
		return
	}
	builds, _ := h.state.Store.ListPackBuilds(pack.ID)
	names := map[string]bool{}
	for _, b := range builds {
		if b.ModrinthVersionID == versionID {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "buildId": b.ID, "existing": true})
			return
		}
		names[b.VersionString] = true
	}
	prov, err := h.state.BuildModpackStorageProvider()
	if err != nil || prov == nil {
		handlers.SendJSONError(w, "No pack storage configured (Settings -> Modpacks)", http.StatusFailedDependency)
		return
	}
	result, ierr := h.importVersion(r.Context(), prov, pack, version, names)
	if ierr != nil {
		log.Printf("packs: import of modrinth version %s into pack %d failed: %s", versionID, pack.ID, ierr.msg)
		handlers.SendJSONError(w, ierr.msg, ierr.status)
		return
	}
	h.audit(r, pack.OwnerID, pack.ID, "build.imported", map[string]any{"modrinthVersion": versionID, "buildId": result.BuildID})
	publishPackEvent(r, h.state, "pack_builds.changed", pack.ID, map[string]interface{}{"packId": pack.ID})
	_ = json.NewEncoder(w).Encode(result)
}

type importVersionResult struct {
	Success  bool     `json:"success"`
	BuildID  int      `json:"buildId"`
	Linked   int      `json:"linked"`
	Uploaded int      `json:"uploaded"`
	Warnings []string `json:"warnings,omitempty"`
}

// importVersion builds one frozen, imported build. Anything that fails after
// the build row exists removes the build again: half an import is worse than
// none, because it looks like the real version.
func (h *PacksHandler) importVersion(ctx context.Context, prov modpack.ModpackStorageProvider, pack *models.Pack, v *services.ModrinthProjectVersion, names map[string]bool) (*importVersionResult, *uploadError) {
	file := v.PrimaryMrpack()
	if file == nil {
		return nil, upErr(http.StatusUnprocessableEntity, "Version "+v.VersionNumber+" has no .mrpack file.")
	}
	data, err := services.DownloadAllowed(ctx, file.URL, maxImportMrpackBytes)
	if err != nil {
		return nil, upErr(http.StatusBadGateway, "Could not download the pack file: "+err.Error())
	}
	if want := file.Hashes["sha1"]; want != "" && !strings.EqualFold(want, sha1hex(data)) {
		return nil, upErr(http.StatusBadGateway, "The downloaded pack file does not match Modrinth's checksum.")
	}
	mrpack, err := services.ParseMrpackFull(data)
	if err != nil {
		return nil, upErr(http.StatusUnprocessableEntity, "The pack file could not be read: "+err.Error())
	}

	loader, loaderVersion := services.MrpackLoader(mrpack.Dependencies)
	if loader == "" {
		loader = "vanilla"
	}
	minecraft := mrpack.Dependencies["minecraft"]
	if minecraft == "" && len(v.GameVersions) > 0 {
		minecraft = v.GameVersions[0]
	}
	channel := models.ChannelBeta
	if v.VersionType == "release" {
		channel = models.ChannelRelease
	}
	name := strings.TrimSpace(v.VersionNumber)
	if name == "" || names[name] {
		name = strings.TrimSpace(name + " (" + v.ID + ")")
	}
	build := &models.PackBuild{PackID: pack.ID, VersionString: name, Minecraft: minecraft, Loader: loader, LoaderVersion: loaderVersion, Changelog: v.Changelog, Channel: models.ChannelDraft, ImportedFrom: "modrinth"}
	buildID, err := h.state.Store.CreatePackBuild(build)
	if err != nil {
		return nil, upErr(http.StatusConflict, "Could not create the build: "+err.Error())
	}
	build.ID = buildID
	fail := func(status int, msg string) (*importVersionResult, *uploadError) {
		if err := h.state.Store.DeletePackBuild(buildID, pack.ID); err != nil {
			log.Printf("packs: half-imported build %d not removed: %v", buildID, err)
		}
		return nil, upErr(status, msg)
	}
	res := &importVersionResult{Success: true, BuildID: buildID}

	// Modrinth files, resolved in bulk.
	ids := []string{}
	for _, f := range mrpack.Files {
		if f.ModrinthVersionID != "" {
			ids = append(ids, f.ModrinthVersionID)
		}
	}
	resolved, err := services.FetchModrinthVersionsBulk(ctx, ids)
	if err != nil {
		return fail(http.StatusBadGateway, "Could not resolve the pack's Modrinth files: "+err.Error())
	}
	for _, f := range mrpack.Files {
		kind := contentTypeForPath(f.Path)
		if mv := resolved[f.ModrinthVersionID]; mv != nil && (f.SHA1 == "" || strings.EqualFold(mv.PrimaryFile().Hashes["sha1"], f.SHA1)) {
			if err := h.addModrinthVersion(pack.OwnerID, build, mv, f.Side, kind); err != nil {
				return fail(http.StatusInternalServerError, "Could not add "+path.Base(f.Path)+": "+err.Error())
			}
			res.Linked++
			continue
		}
		// Not on Modrinth (or not that version's primary file): import the
		// bytes like an upload, from an allowed host only.
		if len(f.Downloads) == 0 {
			res.Warnings = append(res.Warnings, path.Base(f.Path)+" has no download and was skipped.")
			continue
		}
		body, err := services.DownloadAllowed(ctx, f.Downloads[0], maxImportFileBytes)
		if err != nil {
			res.Warnings = append(res.Warnings, path.Base(f.Path)+" could not be downloaded and was skipped: "+err.Error())
			continue
		}
		if f.SHA1 != "" && !strings.EqualFold(f.SHA1, sha1hex(body)) {
			res.Warnings = append(res.Warnings, path.Base(f.Path)+" did not match its checksum and was skipped.")
			continue
		}
		if herr := h.storeImportedFile(ctx, prov, pack, build, f.Path, kind, f.Side, body); herr != nil {
			return fail(herr.status, herr.msg)
		}
		res.Uploaded++
	}
	for _, o := range mrpack.Overrides {
		if !validContentTarget(o.Path) {
			res.Warnings = append(res.Warnings, o.Path+" has an unusable path and was skipped.")
			continue
		}
		if herr := h.storeImportedFile(ctx, prov, pack, build, o.Path, contentTypeForPath(o.Path), o.Side, o.Data); herr != nil {
			return fail(herr.status, herr.msg)
		}
		res.Uploaded++
	}

	// The version's own file is the build's artifact: downloads and installs
	// serve exactly what Modrinth has, not a re-render.
	key := h.mrpackStorageKey(pack, build)
	if err := prov.Put(ctx, key, data); err != nil {
		return fail(http.StatusInternalServerError, "Could not store the pack file: "+err.Error())
	}
	sum := sha256.Sum256(data)
	published := v.DatePublished.UTC()
	build.Channel = channel
	build.Frozen = true
	build.ModrinthPublished = true
	build.ModrinthVersionID = v.ID
	build.MrpackStorageKey = key
	build.MrpackSHA256 = hex.EncodeToString(sum[:])
	build.PublishedAt = &published
	if err := h.state.Store.UpdatePackBuild(build); err != nil {
		return fail(http.StatusInternalServerError, "Could not finish the build: "+err.Error())
	}
	return res, nil
}

// storeImportedFile stores one file of an imported pack the way an upload is
// stored, and attaches it to the build.
func (h *PacksHandler) storeImportedFile(ctx context.Context, prov modpack.ModpackStorageProvider, pack *models.Pack, b *models.PackBuild, targetPath, kind, side string, data []byte) *uploadError {
	if !validContentTarget(targetPath) || !validContentSide(side) {
		return upErr(http.StatusUnprocessableEntity, "Unusable path in pack: "+targetPath)
	}
	fileName := path.Base(targetPath)
	slug := handlers.Slugify(strings.TrimSuffix(fileName, ".jar"))
	if len(slug) > 100 {
		slug = slug[:100]
	}
	// Equal basenames in different folders must not share catalog metadata.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%s/%s", b.PackID, b.ID, kind, targetPath)))
	slug = fmt.Sprintf("%s-%x", slug, sum[:8])
	meta, herr := h.streamWrappedUpload(ctx, prov, &memFile{Reader: bytes.NewReader(data)}, targetPath, pack.OwnerID, slug)
	if herr != nil {
		return herr
	}
	modID, err := h.state.Store.UpsertMod(&models.Mod{OwnerID: pack.OwnerID, Slug: slug, PrettyName: fileName, ContentType: kind})
	if err != nil {
		h.deleteIfUnreferenced(context.WithoutCancel(ctx), prov, meta.key)
		return upErr(http.StatusInternalServerError, "Failed to save "+fileName)
	}
	mvID, err := h.state.Store.CreateModversion(&models.Modversion{
		ModID: modID, Version: meta.version, Filesize: meta.size, StorageKey: meta.key,
		MD5: meta.md5, SHA1: meta.innerSha1, SHA512: meta.innerSha512,
		Source: models.SourceUpload, TargetPath: targetPath,
	})
	if err != nil {
		h.deleteIfUnreferenced(context.WithoutCancel(ctx), prov, meta.key)
		return upErr(http.StatusInternalServerError, "Failed to save "+fileName)
	}
	if _, err := h.state.Store.AttachModversionToBuild(b.ID, mvID, side); err != nil {
		return upErr(http.StatusInternalServerError, "Failed to attach "+fileName)
	}
	return nil
}

// contentTypeForPath reads the content type off a Minecraft-relative path.
func contentTypeForPath(p string) string {
	lower := strings.ToLower(p)
	switch {
	case strings.HasPrefix(lower, "mods/") && strings.HasSuffix(lower, ".jar"):
		return models.ContentTypeMod
	case strings.HasPrefix(lower, "resourcepacks/"):
		return models.ContentTypeResourcepack
	case strings.HasPrefix(lower, "shaderpacks/"):
		return models.ContentTypeShaderpack
	default:
		return models.ContentTypeConfig
	}
}

func sha1hex(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

// memFile lets in-memory bytes take the upload path, which reads a
// multipart.File (Read, ReadAt, Seek, Close).
type memFile struct{ *bytes.Reader }

func (memFile) Close() error { return nil }
