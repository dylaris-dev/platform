package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// modrinthUserAgent identifies Dylaris to the Modrinth API per their UA policy.
// The proxy/deps clients carry their own (unexported, package-local) UA; this
// is the publish flow's copy so it stays independent of those internals.
const modrinthUserAgent = "Dylaris/1.0 (+https://github.com/Bartis-Dev/dylaris-platform)"

type publishModrinthRequest struct {
	Channel        string `json:"channel"`        // "beta"|"release" (draft cannot publish)
	AckNonModrinth bool   `json:"ackNonModrinth"` // user acknowledged the redistribution warning
}

type publishModrinthResponse struct {
	Success           bool     `json:"success"`
	ModrinthProjectID string   `json:"modrinthProjectId"`
	ModrinthVersionID string   `json:"modrinthVersionId"`
	Message           string   `json:"message"`
	Warnings          []string `json:"warnings,omitempty"`
}

// nonModrinthContent returns the target paths of content that is NOT a clean
// Modrinth files[] reference (i.e. will be embedded in overrides/ and needs
// redistribution rights to pass Modrinth moderation).
//
// It asks isMrpackFilesEntry rather than spelling the condition out again: this
// list is what the publisher ACKNOWLEDGES, and the render is what actually
// embeds. A third copy of the same four terms drifting from the other two would
// have the user acknowledge one set while a different set shipped.
func nonModrinthContent(content []models.BuildContentEntry) []string {
	var out []string
	for _, e := range content {
		if !isMrpackFilesEntry(e) {
			out = append(out, e.TargetPath)
		}
	}
	return out
}

func (h *PacksHandler) createModrinthProject(r *http.Request, mc *services.ModrinthClient, pack *models.Pack) (*services.ProjectResponse, error) {
	title := pack.ModrinthProjectName
	if title == "" {
		title = pack.InternalName
	}
	body := pack.Summary
	if body == "" {
		body = title + " — published from Dylaris"
	}
	slug := pack.InternalSlug
	return mc.CreateProject(r.Context(), services.CreateProjectRequest{
		Slug:        slug,
		Title:       title,
		Description: pack.Summary,
		Body:        body,
		ProjectType: "modpack",
		ClientSide:  "required",
		ServerSide:  "required",
		License:     "arr",
		IsDraft:     pack.ModrinthVisibility == "listed",
		Categories:  []string{},
	})
}

// PublishModrinth POST /api/packs/{id}/builds/{buildId}/publish - publishes a
// build to Modrinth under the caller's own personal access token. Without a
// stored PAT the answer is 428, which says the account is not set up rather
// than that the request was wrong.
func (h *PacksHandler) PublishModrinth(w http.ResponseWriter, r *http.Request) {
	if h.pat == nil {
		sendJSONError(w, "Publishing not available", http.StatusServiceUnavailable)
		return
	}
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	buildID, _ := strconv.Atoi(mux.Vars(r)["buildId"])
	userID, _ := r.Context().Value("userID").(string)

	pack, err := h.state.Store.GetPack(packID)
	if err != nil || pack == nil || pack.OwnerID != userID {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	build, err := h.state.Store.GetPackBuild(buildID)
	if err != nil || build == nil || build.PackID != packID {
		sendJSONError(w, "Build not found", http.StatusNotFound)
		return
	}

	var req publishModrinthRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	channel := req.Channel
	if channel == "" {
		channel = build.Channel
	}
	if channel != models.ChannelBeta && channel != models.ChannelRelease {
		sendJSONError(w, "channel must be beta or release", http.StatusBadRequest)
		return
	}

	content, err := h.state.Store.ListBuildContent(buildID)
	if err != nil || len(content) == 0 {
		sendJSONError(w, "Build has no content yet — add some before publishing", http.StatusBadRequest)
		return
	}

	// Non-Modrinth content requires redistribution rights + risks moderation.
	// Warn; require explicit acknowledgement to proceed.
	warnPaths := nonModrinthContent(content)
	if len(warnPaths) > 0 && !req.AckNonModrinth {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(publishModrinthResponse{
			Success:  false,
			Message:  fmt.Sprintf("%d item(s) are not on Modrinth and will be embedded; Modrinth moderation requires redistribution rights. Re-submit with acknowledgement to proceed.", len(warnPaths)),
			Warnings: warnPaths,
		})
		return
	}

	pat, username, err := h.pat.LoadPAT(userID)
	if err != nil {
		sendJSONError(w, "Modrinth PAT not configured: "+err.Error(), http.StatusPreconditionRequired)
		return
	}

	// Freeze the channel + obtain the mrpack bytes. If the build is already
	// frozen and has a stored mrpack (re-publish), reuse the persisted artifact
	// verbatim rather than re-rendering + re-uploading to storage: the storage
	// key is deterministic so re-rendering would be identical work at best and
	// risks diverging from the exact bytes a previous publish shipped. Only
	// render+persist fresh when there is no stored artifact yet.
	build.Channel = channel
	var mrpack []byte
	if build.Frozen && build.MrpackStorageKey != "" {
		prov, perr := h.state.buildModpackStorageProvider()
		if perr != nil || prov == nil {
			sendJSONError(w, "Modpack storage misconfigured; cannot read frozen build", http.StatusInternalServerError)
			return
		}
		mrpack, err = prov.Get(r.Context(), build.MrpackStorageKey)
		if err != nil {
			sendJSONError(w, "Failed to read stored .mrpack: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		mrpack, err = h.persistMrpackForBuild(r.Context(), pack, build, content)
		if err != nil {
			sendJSONError(w, "Failed to build .mrpack: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	mc := services.NewModrinthClient(pat, modrinthUserAgent)

	projectID := pack.ModrinthProjectID
	if projectID == "" {
		created, err := h.createModrinthProject(r, mc, pack)
		if err != nil {
			sendJSONError(w, "Modrinth create project failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		projectID = created.ID
		pack.ModrinthProjectID = projectID
		if err := h.state.Store.UpdatePack(pack); err != nil {
			sendJSONError(w, "Project created ("+projectID+") but local update failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Channel → Modrinth version_type + release status.
	//   beta    → version_type=beta,    status=unlisted (never surfaces publicly)
	//   release → version_type=release, status=listed (unless pack visibility=unlisted)
	versionType := "beta"
	if channel == models.ChannelRelease {
		versionType = "release"
	}
	status := "listed"
	if channel == models.ChannelBeta || pack.ModrinthVisibility == "unlisted" {
		status = "unlisted"
	}
	filename := fmt.Sprintf("%s-%s.mrpack", pack.InternalSlug, build.VersionString)
	created, err := mc.UploadVersion(r.Context(), services.CreateVersionRequest{
		Name:          build.VersionString,
		VersionNumber: build.VersionString,
		Changelog:     build.Changelog,
		Dependencies:  []string{},
		GameVersions:  []string{build.Minecraft},
		VersionType:   versionType,
		Loaders:       []string{modrinthLoaderName(build.Loader)},
		Featured:      channel == models.ChannelRelease,
		Status:        status,
		ProjectID:     projectID,
	}, mrpack, filename)
	if err != nil {
		sendJSONError(w, "Modrinth upload failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	now := time.Now().UTC()
	build.ModrinthVersionID = created.ID
	build.ModrinthPublished = true
	build.PublishedAt = &now
	if err := h.state.Store.UpdatePackBuild(build); err != nil {
		sendJSONError(w, "Published "+created.ID+" but local stamp failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.state.Events.Publish(r.Context(), "pack_builds.changed", map[string]interface{}{"packId": packID})
	h.state.Events.Publish(r.Context(), "packs.changed", map[string]interface{}{"ownerId": userID})
	json.NewEncoder(w).Encode(publishModrinthResponse{
		Success:           true,
		ModrinthProjectID: projectID,
		ModrinthVersionID: created.ID,
		Message:           "Published as " + username + "/" + pack.InternalSlug + " (" + channel + ")",
		Warnings:          warnPaths,
	})
}

// modrinthLoaderName maps a build's loader to Modrinth's version loaders[]
// vocabulary. Modrinth uses bare loader ids ("fabric"/"forge"/"neoforge"/
// "quilt"), NOT the "-loader" suffix the mrpack dependencies block uses.
func modrinthLoaderName(loader string) string {
	switch loader {
	case "fabric-loader":
		return "fabric"
	case "quilt-loader":
		return "quilt"
	default:
		return loader
	}
}
