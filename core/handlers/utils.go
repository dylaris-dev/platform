package handlers

import (
	"encoding/json"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"dylaris-core/services"
)

// slugify turns a display name into a URL-safe slug (lowercase, dashes for
// runs of non-alphanumerics, trimmed, max 64 chars). Shared by the pack
// builder handlers.
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func sendJSONError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"message": message,
	})
}

func IsAdmin(r *http.Request) bool {
	isAdmin, ok := r.Context().Value("isAdmin").(bool)
	return ok && isAdmin
}

// parseUserID extracts a UUID-typed user ID from a mux path variable. By
// default it reads mux.Vars(r)["id"]; pass an alternate var name (e.g.
// "userId") when the route uses one. On failure it writes a 400 response
// and returns ok=false so the caller can early-out.
func parseUserID(w http.ResponseWriter, r *http.Request, varName ...string) (string, bool) {
	key := "id"
	if len(varName) > 0 && varName[0] != "" {
		key = varName[0]
	}
	id := mux.Vars(r)[key]
	if _, err := uuid.Parse(id); err != nil {
		sendJSONError(w, "Invalid user ID", http.StatusBadRequest)
		return "", false
	}
	return id, true
}

// setAttachmentDisposition writes a Content-Disposition header that keeps the
// caller-supplied name INSIDE the filename parameter.
//
// Four handlers built this header by concatenating a name into a quoted string
// (ticket attachments, both file-manager download paths, the library) and one
// of them did not even quote it. A name is not a token: a space ends an
// unquoted one, and a quote closes a quoted one, so `report".pdf` produced
//
//	attachment; filename="report".pdf"
//
// which lets the uploader append their own parameters. RFC 6266 gives
// filename* precedence over filename, so a crafted name decides what the
// DOWNLOADER's browser saves the file as - and on a ticket attachment or a
// library file the uploader and the downloader are different people.
//
// The severity is genuinely low on its own: the uploader already picks the
// visible name, browsers sanitise download names themselves, and the sniffed
// Content-Type plus nosniff still govern what may execute. What it defeats is
// the sanitising each caller does on the name BEFORE this point, which is the
// only reason those sanitisers exist.
//
// mime.FormatMediaType does the quoting and the RFC 2231 encoding for
// non-ASCII, so no caller has to decide when a name needs which. It returns ""
// for a name it cannot encode at all; the fallback is a bare "attachment",
// which loses the suggested filename and nothing else.
//
// Not shared with storage_migration.go's safeFilenamePart: that one builds a
// name Core chose and can flatten to [A-Za-z0-9_-] without losing anything a
// user typed. Here the name IS what the user typed.
func setAttachmentDisposition(w http.ResponseWriter, filename string) {
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": filename}); v != "" {
		w.Header().Set("Content-Disposition", v)
		return
	}
	w.Header().Set("Content-Disposition", "attachment")
}

// sendConnTestFailure answers a failed "Test connection" with the stage
// attached.
//
// The status stays 502 and `success` stays false, because that is what every
// existing caller of these endpoints already reads. What is new is `stage`:
// the panel words its heading from it, so "nothing answered on that address"
// and "answered, then refused" stop arriving as the same red box. See
// services/conncheck.go for why that distinction is the whole feature.
func sendConnTestFailure(w http.ResponseWriter, check services.ConnCheck) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"ok":      false,
		"stage":   check.Stage,
		"message": check.Message,
	})
}
