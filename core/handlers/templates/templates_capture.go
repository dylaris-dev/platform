package templates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/handlers/guard"
	"dylaris-core/handlers/httpx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-pkg/validate"

	pb "dylaris-proto/node"

	"github.com/google/uuid"
)

const (
	// captureSlack is what the node's zip may carry beyond the size limit. The
	// node deflates, so this bounds the compressed stream; extractTemplateZip
	// enforces the real limit on the decompressed bytes.
	captureSlack = 1 << 20
	// maxCaptureSelected bounds the request, not the server: a selection is
	// a handful of folders and files, not a listing of the tree.
	maxCaptureSelected = 500
)

var (
	errNode           = errors.New("template: the node refused the read")
	errNodeIncomplete = errors.New("template: the node stopped before the zip was complete")
)

// writeNodeStream copies the chunks of a node transfer to dst until the final
// TransferDone. A stream that closes without it, a node error, or more than
// max bytes is an error; max == nil is unbounded.
func writeNodeStream(ctx context.Context, ch <-chan *pb.NodeMessage, dst io.Writer, max *int64) error {
	var n int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case resp, open := <-ch:
			if !open {
				return errNodeIncomplete
			}
			if e := resp.GetError(); e != nil {
				return fmt.Errorf("%w: %d %s", errNode, e.Code, e.Message)
			}
			if chunk := resp.GetChunk(); chunk != nil {
				n += int64(len(chunk.Data))
				if services.Exceeds(max, n) {
					return errTooLarge
				}
				if _, err := dst.Write(chunk.Data); err != nil {
					return fmt.Errorf("template: write capture: %w", err)
				}
			}
			if nodegrpc.IsFinalTransferDone(resp) {
				return nil
			}
		}
	}
}

// fetchSelection has the node zip the selected paths under basePath and
// stages the stream in a temp file; the caller removes it.
func fetchSelection(ctx context.Context, d Deps, nodeID int, serverUUID, basePath string, selected []string, max *int64) (string, error) {
	if d.GRPCRegistry == nil {
		return "", errors.New("template: gRPC registry not initialized")
	}
	reqID := uuid.NewString()
	ch, err := d.GRPCRegistry.SendRequestStreaming(nodeID, &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload: &pb.NodeMessage_SelectiveReadReq{
			SelectiveReadReq: &pb.SelectiveReadReq{BasePath: basePath, Selected: selected},
		},
	})
	if err != nil {
		return "", fmt.Errorf("template: ask node %d: %w", nodeID, err)
	}
	defer d.GRPCRegistry.CleanupRequest(nodeID, reqID)

	tmp, err := os.CreateTemp("", "template-capture-*.zip")
	if err != nil {
		return "", fmt.Errorf("template: stage capture: %w", err)
	}
	fail := func(err error) (string, error) {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := writeNodeStream(ctx, ch, tmp, captureBound(max)); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	return tmp.Name(), nil
}

func captureBound(max *int64) *int64 {
	b := int64(maxUploadBody)
	if max != nil && *max+captureSlack < b {
		b = *max + captureSlack
	}
	return &b
}

type captureRequest struct {
	templateMeta
	ServerID  int      `json:"serverId"`
	SubServer string   `json:"subServer"`
	Selected  []string `json:"selected"`
}

// Capture POST /api/templates/capture - makes a template from files of a
// running server. The caller needs files.read on that server and the same
// create rule as any template; nothing is kept when the capture fails.
func (h *TemplatesHandler) Capture(w http.ResponseWriter, r *http.Request) {
	if caller, _ := h.viewer(r, nil); h.refuseSuspended(w, r, caller.UserID) {
		return
	}
	if !httpx.CapBody(w, r, 64<<10) {
		return
	}
	var req captureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	// A missing server and one the caller may not read look the same.
	res, err := h.d.Authz.Resolve(authz.IdentityFromContext(r.Context()), req.ServerID)
	if req.ServerID <= 0 || err != nil || !res.HasCap("files.read") {
		httpx.SendJSONError(w, "You may not read files on that server", http.StatusForbidden)
		return
	}
	if !validate.IsSubServerName(req.SubServer) || len(req.Selected) == 0 || len(req.Selected) > maxCaptureSelected {
		httpx.SendJSONError(w, "Choose a sub-server and at least one path to capture", http.StatusBadRequest)
		return
	}
	for _, p := range req.Selected {
		if !validate.IsSafeRelPath(p) {
			httpx.SendJSONError(w, "Invalid path", http.StatusBadRequest)
			return
		}
	}
	srv, err := h.d.Store.GetServerByID(req.ServerID)
	if err != nil || srv == nil {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	h.fillFromInstall(&req, srv)

	t, s, ok := h.newTemplate(w, r, req.templateMeta)
	if !ok {
		return
	}
	zipPath, err := fetchSelection(r.Context(), h.d, srv.NodeID, srv.UUID, req.SubServer, req.Selected, requestBound(s, t.BackupStorageID != nil))
	if err != nil {
		note := h.discard(t)
		switch {
		case errors.Is(err, errTooLarge):
			tooLargeNote(w, note)
		case errors.Is(err, errNode), errors.Is(err, errNodeIncomplete):
			log.Printf("templates: capture from server %d: %v", srv.ID, err)
			httpx.SendJSONError(w, withNote("The server's node could not read those files.", note), http.StatusBadGateway)
		default:
			log.Printf("templates: capture from server %d: %v", srv.ID, err)
			httpx.SendJSONError(w, withNote("Could not capture the files.", note), http.StatusInternalServerError)
		}
		return
	}
	defer os.Remove(zipPath)
	skipped, err := extractTemplateZip(r.Context(), h.d, t, zipPath, s)
	if err != nil {
		h.extractFailed(w, t, err, h.discard(t))
		return
	}
	v, _ := h.viewer(r, t)
	body := map[string]interface{}{"template": item(*t, v, s.MaxSizeBytes), "skipped": skipped}
	if private := services.TemplatePrivacyPaths(req.Selected); len(private) > 0 {
		body["warning"] = "The template includes " + strings.Join(private, ", ") + ", which can hold player names and IP addresses. Remove them before you share the template."
	}
	writeJSON(w, http.StatusCreated, body)
}

// fillFromInstall defaults empty software and versions to what the sub-server
// was installed as. A missing record leaves them empty. The software version
// is the loader actually installed (installedLoader), so the template installs
// the loader its files were made with rather than whatever is newest that day.
func (h *TemplatesHandler) fillFromInstall(req *captureRequest, srv *models.Server) {
	if req.Software != "" && req.McVersion != "" && req.SoftwareVersion != "" {
		return
	}
	rec, err := h.d.Store.GetSubServerInstall(req.ServerID, req.SubServer)
	if err != nil || rec == nil {
		return
	}
	// A record of another source (upload, modpack) is not software a template
	// can install; the caller picks one instead.
	if req.Software == "" && slices.Contains(services.TemplateSoftware, rec.InstallerType) {
		req.Software = rec.InstallerType
	}
	if strings.ToLower(strings.TrimSpace(req.Software)) != rec.InstallerType {
		return
	}
	if req.McVersion == "" {
		req.McVersion = services.RecordedMinecraftVersion(rec.InstallerType, rec.McVersion, rec.BuildVersion, rec.Loader)
	}
	if req.SoftwareVersion == "" {
		req.SoftwareVersion = h.installedLoader(req, rec, srv)
	}
}

// listServerDir is guard.ListServerDirFor, swapped in tests.
var listServerDir = guard.ListServerDirFor

// installedLoader is the loader version the sub-server runs: the recorded one,
// else the one its library folder holds, else the recorded build (for an
// online Forge or Fabric install that is the Minecraft version, which installs
// the newest loader).
func (h *TemplatesHandler) installedLoader(req *captureRequest, rec *models.SubServerInstall, srv *models.Server) string {
	dir := services.TemplateLoaderLibraryDir(rec.InstallerType)
	if dir == "" {
		return rec.BuildVersion
	}
	if rec.Loader != "" {
		return rec.Loader
	}
	entries, err := listServerDir(h.d.GRPCRegistry, srv.NodeID, srv.UUID, path.Join(req.SubServer, dir))
	if err != nil {
		log.Printf("templates: loader of server %d/%s: %v", srv.ID, req.SubServer, err)
		return rec.BuildVersion
	}
	var names []string
	for _, e := range entries {
		if e != nil && e.IsDir {
			names = append(names, e.Name)
		}
	}
	if v := services.TemplateLoaderFromLibraries(rec.InstallerType, req.McVersion, names); v != "" {
		return v
	}
	return rec.BuildVersion
}
