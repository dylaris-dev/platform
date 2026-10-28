package packs

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

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
	description := strings.TrimSpace(pack.Summary)
	if len([]rune(description)) < 3 {
		description = title + " modpack"
	}
	if text := []rune(description); len(text) > 255 {
		description = string(text[:255])
	}
	return mc.CreateProject(r.Context(), services.CreateProjectRequest{
		Slug:        slug,
		Title:       title,
		Description: description,
		Body:        body,
		ProjectType: "modpack",
		ClientSide:  "required",
		ServerSide:  "required",
		License:     services.ModrinthAllRightsReserved,
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
		handlers.SendJSONError(w, "Publishing not available", http.StatusServiceUnavailable)
		return
	}
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	buildID, _ := strconv.Atoi(mux.Vars(r)["buildId"])

	pack, err := h.state.Store.GetPack(packID)
	if err != nil || pack == nil || !h.mayEditPack(r, pack) {
		handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	build, err := h.state.Store.GetPackBuild(buildID)
	if err != nil || build == nil || build.PackID != packID {
		handlers.SendJSONError(w, "Build not found", http.StatusNotFound)
		return
	}
	if build.ModrinthPublished {
		handlers.SendJSONError(w, "This build is already published. Create a new build for changes.", http.StatusConflict)
		return
	}

	var req publishModrinthRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	channel := req.Channel
	if channel == "" {
		channel = build.Channel
	}
	if channel != models.ChannelBeta && channel != models.ChannelRelease {
		handlers.SendJSONError(w, "channel must be beta or release", http.StatusBadRequest)
		return
	}

	content, err := h.state.Store.ListBuildContent(buildID)
	if err != nil || len(content) == 0 {
		handlers.SendJSONError(w, "Build has no content yet — add some before publishing", http.StatusBadRequest)
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

	// Each team member publishes with their OWN token; Modrinth checks their
	// upload right again (owner decision 2026-10-28).
	caller, _ := r.Context().Value("userID").(string)
	pat, username, err := h.pat.LoadPAT(caller)
	if err != nil {
		handlers.SendJSONError(w, "Connect your Modrinth token first (Account, Modrinth).", http.StatusPreconditionRequired)
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
		prov, perr := h.state.BuildModpackStorageProvider()
		if perr != nil || prov == nil {
			handlers.SendJSONError(w, "Modpack storage misconfigured; cannot read frozen build", http.StatusInternalServerError)
			return
		}
		mrpack, err = prov.Get(r.Context(), build.MrpackStorageKey)
		if err != nil {
			handlers.SendJSONError(w, "Failed to read stored .mrpack: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		mrpack, err = h.persistMrpackForBuild(r.Context(), pack, build, content)
		if err != nil {
			handlers.SendJSONError(w, "Failed to build .mrpack: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	mc := services.NewModrinthClient(pat)

	projectID := pack.ModrinthProjectID
	if projectID == "" {
		created, err := h.createModrinthProject(r, mc, pack)
		if err != nil {
			handlers.SendJSONError(w, "Modrinth create project failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		projectID = created.ID
		pack.ModrinthProjectID = projectID
		if err := h.state.Store.UpdatePack(pack); err != nil {
			handlers.SendJSONError(w, "Project created ("+projectID+") but local update failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// The pack now follows the new project's team; the caller just
		// created it, so they own it there.
		h.grantMembership(caller, projectID, services.ModrinthPermAll)
	}

	// Channel → Modrinth version_type. Every version is uploaded "listed": an
	// unlisted one never shows in the project's version list, not even to its
	// owner, which read as "publish did nothing" (owner report 2026-10-28; every
	// pack defaults to modrinth_visibility=unlisted, so releases were hidden
	// too). Privacy before launch comes from the draft project, which only its
	// members see until it is submitted for review; beta stays marked beta.
	versionType := "beta"
	if channel == models.ChannelRelease {
		versionType = "release"
	}
	status := "listed"
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
		handlers.SendJSONError(w, "Modrinth upload failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	now := time.Now().UTC()
	build.ModrinthVersionID = created.ID
	build.ModrinthPublished = true
	build.PublishedAt = &now
	if err := h.state.Store.UpdatePackBuild(build); err != nil {
		handlers.SendJSONError(w, "Published "+created.ID+" but local stamp failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Modrinth's checklist blocks review while a version's environment is
	// unknown, and a v2 upload cannot set it.
	message := "Published as " + username + "/" + pack.InternalSlug + " (" + channel + ")"
	page, err := mc.GetProjectPage(r.Context(), projectID)
	if err == nil {
		err = setUploadedVersionEnvironment(r.Context(), mc, page.Versions, created.ID)
	}
	if err != nil {
		log.Printf("packs: environment of modrinth version %s not set: %v", created.ID, err)
		message += ". Set its environment on the pack's Modrinth page."
	}

	publishPackEvent(r, h.state, "pack_builds.changed", packID, map[string]interface{}{"packId": packID})
	h.state.Events.Publish(r.Context(), "packs.changed", map[string]interface{}{"ownerId": pack.OwnerID})
	json.NewEncoder(w).Encode(publishModrinthResponse{
		Success:           true,
		ModrinthProjectID: projectID,
		ModrinthVersionID: created.ID,
		Message:           message,
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
