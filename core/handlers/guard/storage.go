package guard

import (
	"encoding/json"
	"errors"
	"net/http"

	"dylaris-core/handlers/httpx"
	"dylaris-core/services/corestorage"
	"dylaris-core/storage"
)

// CoreStorageUnavailableResponse writes a 503 telling the caller Core file
// storage must be configured (or is broken) before this endpoint can work.
// Every handler that resolves a provider per-request via
// BuildProvider calls this on failure instead of proceeding with
// a nil provider.
//
// Deliberately NOT featureDisabledResponse/FeatureCoreStorage's exact shape
// (no X-Feature-Disabled header, a different "error" value): that response is
// reserved for RequireCoreStorageConfigured, the router-level gate wrapping
// EXACTLY the 4 WRITE routes, which fires before the request body is even
// parsed. This is the per-request backstop reached from inside a handler
// body - including READ handlers, which the gate never wraps - so keeping
// the two responses distinguishable preserves that route-coverage invariant
// instead of muddying it.
//
// err splits the two ways a per-request build fails, because they call for
// opposite actions from the operator. "Not configured" means go and configure
// it. UNREACHABLE means the config is fine and Core cannot see the storage
// right now, so there is nothing to fix in the settings form. Reporting the
// second as the first sends the operator to rewrite a working config; a plain
// 500 tells them nothing; and a 404 would be an outright lie, claiming the
// file is gone when the truth is that Core cannot see the mount. Both keep the
// shape this response already had, so the distinction from featureDisabledResponse
// described above is unaffected.
func CoreStorageUnavailableResponse(w http.ResponseWriter, err error) {
	code := "core_storage_unavailable"
	msg := "Core file storage is not configured. Configure it under Settings -> Core file storage."
	if errors.Is(err, storage.ErrBackendUnreachable) {
		code = "core_storage_unreachable"
		msg = "Core file storage is UNREACHABLE: the configured path is not answering. The configuration itself is unchanged; check that the mount is still up on the Core host."
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   code,
		"message": msg,
	})
}

// SendReachRefusal writes a refusal as JSON, including the per-Core round
// result when there is one so the panel can name the failing Cores. Here rather
// than on CoreStorageHandler: it reads corestorage.SharedStorageRefusal's fields.
func SendReachRefusal(w http.ResponseWriter, err error) {
	var refusal *corestorage.SharedStorageRefusal
	if !errors.As(err, &refusal) {
		httpx.SendJSONError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(refusal.Status)
	body := map[string]interface{}{"success": false, "message": refusal.Message}
	if refusal.Result != nil {
		body["round"] = refusal.Result
	}
	_ = json.NewEncoder(w).Encode(body)
}
