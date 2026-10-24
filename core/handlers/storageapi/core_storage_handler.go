package storageapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"dylaris-core/handlers"

	"dylaris-core/services"
	"dylaris-core/services/storagereach"
)

// pathOnContainerRootFS is this package's seam for the ephemeral-path warning
// (tests replace it); it points at the check handlers' core storage uses.
var pathOnContainerRootFS = handlers.PathIsOnContainerRootFS

// CoreStorageHandler serves the shared Core file storage config CRUD + probe.
type CoreStorageHandler struct {
	state *handlers.AppState
}

func NewCoreStorageHandler(state *handlers.AppState) *CoreStorageHandler {
	return &CoreStorageHandler{state: state}
}

// GetConfig GET /api/settings/core-storage - PANEL settings.read (RequireCap
// at the route). The stored S3 secret is never emitted: S3SecretKey is
// blanked (and, thanks to its "omitempty" json tag, omitted entirely from
// the response) while S3SecretSet tells the panel one is already stored.
// GetConfig GET /api/settings/core-storage - PANEL settings.read (RequireCap
// at the route). Alongside the stored config it reports how many Cores are
// online, which the panel's round-progress counter uses as its expected
// total once a save starts.
func (h *CoreStorageHandler) GetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := h.state.LoadCoreStorageConfig()
	cfg.S3SecretKey = "" // write-only; never emitted

	// The instance ids are NOT emitted, only the count. They are hostnames, and
	// the count alone is everything the form has to render.
	online, err := h.state.CountOnlineCores(r.Context())
	if err != nil {
		// A hint the UI could not compute is not fatal to the response: the
		// counter just starts from 0 until a save round supplies a live total.
		log.Printf("core storage: could not count online Cores for the settings form: %v", err)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"settings":    cfg,
		"onlineCores": online,
	})
}

// SaveConfig POST /api/settings/core-storage - PANEL settings.write
// (RequireCap at the route). Validates before persisting anything, so a
// rejected save never partially overwrites a previously-good config.
func (h *CoreStorageHandler) SaveConfig(w http.ResponseWriter, r *http.Request) {
	var req handlers.CoreStorageConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req = handlers.NormalizeCoreStorageCandidate(req)

	// A selected connection implies the s3 backend. Set it before the merge:
	// mergeCoreStorageCandidate discards a candidate with an empty backend, which
	// would silently drop the connection selection.
	if req.ConnectionID != 0 && req.Backend == "" {
		req.Backend = "s3"
	}

	existing := h.state.LoadCoreStorageConfig()
	effective := handlers.MergeCoreStorageCandidate(req, existing)

	if effective.ConnectionID != 0 {
		// The connection supplies the credentials, so validate the RESOLVED
		// connection rather than the inline s3 fields (which the operator may
		// deliberately leave blank).
		conn, err := h.state.Store.GetStorageConnection(effective.ConnectionID)
		if err != nil {
			handlers.SendJSONError(w, "Selected storage connection not found", http.StatusBadRequest)
			return
		}
		connCfg := handlers.CoreStorageConfigFromConnection(conn)
		if err := handlers.ValidateCoreStorageConfig(connCfg); err != nil {
			handlers.SendJSONError(w, "Selected storage connection is not usable: "+err.Error(), http.StatusBadRequest)
			return
		}
		// After validation, before any write: a saved connection is still
		// shared storage every Core has to reach, exactly like the inline
		// branch below. connCfg, not effective: effective's inline s3 fields
		// are typically blank for a connection-backed save, and the round
		// needs the connection's real credentials to actually probe with.
		if err := h.state.CheckSharedStorageReachable(r.Context(), connCfg); err != nil {
			h.sendReachRefusal(w, err)
			return
		}
	} else {
		if err := handlers.ValidateCoreStorageConfig(effective); err != nil {
			handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
		// After validation, before any write: a refused save must never leave
		// a half-applied config behind. The round PROVES shared access rather
		// than inferring it from a Core count, so it also covers s3.
		if err := h.state.CheckSharedStorageReachable(r.Context(), effective); err != nil {
			h.sendReachRefusal(w, err)
			return
		}
	}

	// req.S3SecretKey, not effective.S3SecretKey: the merged config carries the
	// STORED secret whenever the request omitted the backend, so passing the
	// merged value would turn a secret-only rotation into a no-op that still
	// answers 200.
	if err := h.state.PersistCoreStorageConfig(effective, req.S3SecretKey); err != nil {
		// The wrapped error carries the raw store error; the response names
		// only the settings key, since settings.write is a delegatable panel
		// capability and DB error text is not for it.
		log.Printf("core storage: save config: %v", err)
		msg := "Failed to save setting"
		var writeErr *handlers.CoreStorageSettingWriteError
		if errors.As(err, &writeErr) {
			msg = "Failed to save setting: " + writeErr.Key
		}
		handlers.SendJSONError(w, msg, http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// TestConnection POST /api/settings/core-storage/test - PANEL settings.write
// (RequireCap at the route). Builds a provider from the CANDIDATE config in
// the request body (not the saved one) so an admin can test before saving;
// an empty/absent body falls back to testing the currently-saved config, and
// a blank secret in a submitted candidate reuses the stored one, mirroring
// SaveConfig. The probe writes, reads back and deletes a uniquely-named
// object under a "_probe" sub-prefix so it never touches real Library/ticket
// data.
func (h *CoreStorageHandler) TestConnection(w http.ResponseWriter, r *http.Request) {
	// The stage rides along with every verdict. A wrong endpoint and a wrong
	// secret key are both "connection test failed" without it, and they are
	// fixed by different people - see services/conncheck.go.
	respondStage := func(ok bool, msg, stage string) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "ok": ok, "message": msg, "stage": stage,
		})
	}
	respond := func(ok bool, msg string) {
		stage := services.StageOK
		if !ok {
			stage = services.StageRejected
		}
		respondStage(ok, msg, stage)
	}
	respondWarn := func(ok bool, msg, warning string) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "ok": ok, "message": msg, "warning": warning, "stage": services.StageOK,
		})
	}

	var candidate handlers.CoreStorageConfig
	if err := json.NewDecoder(r.Body).Decode(&candidate); err != nil && err != io.EOF {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	candidate = handlers.NormalizeCoreStorageCandidate(candidate)
	effective := handlers.MergeCoreStorageCandidate(candidate, h.state.LoadCoreStorageConfig())

	if err := handlers.ValidateCoreStorageConfig(effective); err != nil {
		respond(false, err.Error())
		return
	}
	// For an S3 backend, ask whether the endpoint answers at all before signing
	// anything. A dead endpoint and a rejected key produce the same red box
	// otherwise, and the key is what people retype first.
	if effective.Backend == "s3" {
		if host, port, ok := services.HostPortFromEndpoint(effective.S3Endpoint); ok {
			if reach := services.Reachable(r.Context(), host, port); !reach.OK {
				respondStage(false, reach.Message, reach.Stage)
				return
			}
		}
	}

	// nil gate: the candidate may name a completely different path from the
	// live one, so the live gate's verdict says nothing about it. An operator
	// who asks to test a path is also entitled to have it actually tested
	// rather than answered from a cached verdict.
	prov, err := handlers.NewStorageProviderForConfig(effective, "_probe", nil, nil)
	if err != nil {
		respond(false, "Could not build provider: "+err.Error())
		return
	}
	ok, msg := handlers.ProbeStorageProvider(r.Context(), prov)
	if !ok {
		// A local path never got the reachability step above, and its failures
		// are filesystem ones; the classifier passes those through unchanged.
		check := services.ClassifyStorageFailure(msg)
		respondStage(false, check.Message, check.Stage)
		return
	}
	if effective.Backend == "path" || effective.Backend == "local" {
		// newStorageProviderForConfig(effective, "_probe") MkdirAll'd this
		// exact directory; probeStorageProvider cleans up the object inside
		// it on every return path, so it is empty by now. os.Remove (never
		// RemoveAll) only succeeds on an empty dir, so this can't remove
		// anything else - best-effort, so a tested-then-rejected/never-saved
		// path doesn't keep an empty "_probe" dir behind forever.
		_ = os.Remove(filepath.Join(effective.Path, "_probe"))

		// The probe above passes on an UNMOUNTED path: a directory that only
		// exists in the container's own writable layer is perfectly writable,
		// readable and consistent, so every check this handler performs
		// succeeds while the data silently disappears on the next container
		// recreation. Reporting that as a plain green result is the trap.
		//
		// A warning, not a failure: an ephemeral path is a legitimate choice
		// for a throwaway dev instance, and this handler must not be the thing
		// that decides otherwise.
		if onRoot, determinable := pathOnContainerRootFS(effective.Path); determinable && onRoot {
			respondWarn(ok, msg, "This path is on the container's own filesystem, not a mounted volume, so everything written here is LOST when the container is recreated. The read/write test above still passes because that directory is genuinely writable. To keep the data, add a bind mount or volume for this path in your compose/stack file, or point the path at a directory inside a volume that is already mounted.")
			return
		}
	}
	respond(ok, msg)
}

// sendReachRefusal writes a refusal as JSON, including the per-Core round
// result when there is one so the panel can name the failing Cores.
func (h *CoreStorageHandler) sendReachRefusal(w http.ResponseWriter, err error) {
	handlers.SendReachRefusal(w, err)
}

// StorageReachStatus GET /api/settings/storage-reach - PANEL settings.read.
// The fleet storage-health surface: which Cores are currently failing their
// self-check, and which are online at all.
//
// READ-ONLY by design: it must never trigger a check itself. Service.SelfCheck
// is documented as unsafe to call concurrently with the service's own running
// Start loop - both paths end in apply, whose state (notSharedStreak,
// lastStatus) is single-writer by convention and not mutex-guarded - so this
// handler only reads what the periodic loop and any in-flight config round
// have already recorded in Redis.
func (h *CoreStorageHandler) StorageReachStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	faults, err := storagereach.Faults(ctx, h.state.Redis)
	if err != nil {
		handlers.SendJSONError(w, "Could not read storage health", http.StatusServiceUnavailable)
		return
	}
	online, err := services.OnlineCoreIDs(ctx, h.state.Redis)
	if err != nil {
		handlers.SendJSONError(w, "Could not read the online Core set", http.StatusServiceUnavailable)
		return
	}
	if faults == nil {
		// [] rather than null: the panel maps over this directly.
		faults = []storagereach.Fault{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"faults":      faults,
		"onlineCores": online,
	})
}
