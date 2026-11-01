package servers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	pb "dylaris-proto/node"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
)

// templateProxySince is the release that ships the node change installing a
// proxy over an upload (node/installer.go uploadSoftware). It must match that
// release's version: if the release moves to another CalVer, move this too.
const templateProxySince = "2026.11.01"

const proxyTemplateRefused = "This node runs an older release that cannot install a proxy template. Update the node, then try again."

// templatePushLockTTL only bounds a lock a crashed Core leaves behind; a large
// template pushed to a slow node can take minutes, far past the 30 s cooldown.
const templatePushLockTTL = 15 * time.Minute

const templatePushBusy = "A template is already being copied to this server. Try again when it finishes."

const templatePushUnlockScript = `
if redis.call('get', KEYS[1]) == ARGV[1] then
	return redis.call('del', KEYS[1])
else
	return 0
end
`

// lockTemplatePush claims the per-server push lock, for admins too: they skip
// the install cooldown, and two pushes into one directory interleave their
// writes. release deletes the key only while it still holds this claim's
// token, so a lock that expired and was re-taken is left to its new holder.
func lockTemplatePush(ctx context.Context, rdb *redis.Client, serverUUID string) (release func(), ok bool, err error) {
	key := fmt.Sprintf("dylaris:server:%s:template-push", serverUUID)
	token := uuid.NewString()
	ok, err = rdb.SetNX(ctx, key, token, templatePushLockTTL).Result()
	if err != nil || !ok {
		return nil, false, err
	}
	return func() {
		if err := rdb.Eval(context.Background(), templatePushUnlockScript, []string{key}, token).Err(); err != nil {
			log.Printf("template install: release push lock of %s: %v", serverUUID, err)
		}
	}, true, nil
}

// templatePush is pushTemplateZip, replaceable in tests (there is no node).
var templatePush = pushTemplateZip

// resolveTemplateInstall turns installer type "template" into the upload-zip
// install of the template's files, before any later check of SetupServer reads
// the installer, so each of them applies unchanged. Answers the request and
// returns nil when it is refused.
func (h *ServerHandler) resolveTemplateInstall(w http.ResponseWriter, r *http.Request, req *SetupServerRequest) *models.Template {
	in := &req.Installer
	if in.TemplateID <= 0 {
		handlers.SendJSONError(w, "installer.templateId is required for template", http.StatusBadRequest)
		return nil
	}
	// The template decides the software and the layout; a field that would
	// override it is refused rather than silently dropped.
	if in.Version != "" || in.McVersion != "" || in.Loader != "" || in.URL != "" || in.Structure != "" ||
		in.Software != "" || in.ModrinthProjectID != "" || in.ModrinthVersionID != "" || in.ModrinthProjectSlug != "" ||
		in.PackID != 0 || in.BuildID != 0 || in.TechnicSlug != "" || in.TechnicBuild != "" {
		handlers.SendJSONError(w, "A template install takes templateId and wipePaths only", http.StatusBadRequest)
		return nil
	}
	t, ok := h.visibleTemplate(w, r, in.TemplateID)
	if !ok {
		return nil
	}
	typ, software, version, mcVersion, loader, err := services.TemplateInstallPlan(*t)
	if err != nil {
		handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
		return nil
	}
	in.Type, in.Software, in.Version, in.McVersion, in.Loader, in.Structure = typ, software, version, mcVersion, loader, "direct"
	return t
}

// visibleTemplate loads a template the caller may install. One they may not
// see is a 404 like a missing one, as in the templates API, so ids cannot be
// probed. TemplateCanSee also applies the feature's audience to non-admins.
func (h *ServerHandler) visibleTemplate(w http.ResponseWriter, r *http.Request, id int) (*models.Template, bool) {
	t, err := h.state.Store.GetTemplate(id)
	if err != nil {
		log.Printf("template install: get %d: %v", id, err)
		handlers.SendJSONError(w, "Could not load the template", http.StatusInternalServerError)
		return nil, false
	}
	if t == nil {
		handlers.SendJSONError(w, "Template not found", http.StatusNotFound)
		return nil, false
	}
	v, err := h.state.TemplateViewer(r, t)
	if err != nil {
		log.Printf("template install: viewer for %d: %v", id, err)
		handlers.SendJSONError(w, "Could not load the template", http.StatusInternalServerError)
		return nil, false
	}
	if !services.TemplateCanSee(*t, v, services.LoadTemplateSettings(r.Context(), h.state.FeatureFlags)) {
		handlers.SendJSONError(w, "Template not found", http.StatusNotFound)
		return nil, false
	}
	return t, true
}

// pushTemplateZip writes the template's files as <subServer>/.upload.zip on
// the node, where the upload-zip installer reads them: the same write sequence
// as a browser upload (FileHandler.UploadFileHandler). The node is never handed
// a URL. The zip is built into a temp file first so TotalSize is exact.
func pushTemplateZip(ctx context.Context, state *handlers.AppState, nodeID int, serverUUID, subServer string, t *models.Template) error {
	if state.GRPCRegistry == nil {
		return errors.New("node registry not available")
	}
	conn, ok := state.GRPCRegistry.GetConnection(nodeID)
	if !ok {
		return fmt.Errorf("node %d not connected", nodeID)
	}

	tmp, err := os.CreateTemp("", "template-*.zip")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := state.TemplateZip(ctx, t, tmp); err != nil {
		return fmt.Errorf("building the archive: %w", err)
	}
	size, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	reqID := uuid.NewString()
	resp, err := state.GRPCRegistry.SendRequest(nodeID, &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_WriteReq{WriteReq: &pb.WriteFileReq{Path: subServer + "/.upload.zip", TotalSize: size}},
	}, 30*time.Second)
	if err != nil {
		return fmt.Errorf("node write init: %w", err)
	}
	if e := resp.GetError(); e != nil {
		return fmt.Errorf("node write: %s", e.Message)
	}
	// Listening before the first chunk, so the node's verdict cannot be missed.
	wait, stop := conn.AwaitReply(reqID)
	defer stop()

	buf := make([]byte, 64*1024)
	var offset int64
	for {
		n, rerr := tmp.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := conn.Send(&pb.NodeMessage{
				RequestId:  reqID,
				ServerUuid: serverUUID,
				Payload:    &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: chunk, Offset: offset}},
			}); err != nil {
				return fmt.Errorf("node write chunk: %w", err)
			}
			offset += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("reading the archive: %w", rerr)
		}
	}
	if err := conn.Send(&pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{TotalBytes: offset, Filename: ".upload.zip"}},
	}); err != nil {
		return fmt.Errorf("node write finalize: %w", err)
	}
	if _, msg, ok := handlers.ConfirmNodeWrite(wait); !ok {
		return fmt.Errorf("node write: %s", msg)
	}
	return nil
}
