package httpx

import (
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

func SendJSONError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"message": message,
	})
}

// SendJSONErrorCode is SendJSONError plus a stable machine-readable code. The
// panel needs to tell the three denials apart to render the right call to
// action, and matching on the prose would break the first time it is reworded.
func SendJSONErrorCode(w http.ResponseWriter, message, code string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"message": message,
		"code":    code,
	})
}

// ParseUserID extracts a UUID-typed user ID from a mux path variable. By
// default it reads mux.Vars(r)["id"]; pass an alternate var name (e.g.
// "userId") when the route uses one. On failure it writes a 400 response
// and returns ok=false so the caller can early-out.
func ParseUserID(w http.ResponseWriter, r *http.Request, varName ...string) (string, bool) {
	key := "id"
	if len(varName) > 0 && varName[0] != "" {
		key = varName[0]
	}
	id := mux.Vars(r)[key]
	if _, err := uuid.Parse(id); err != nil {
		SendJSONError(w, "Invalid user ID", http.StatusBadRequest)
		return "", false
	}
	return id, true
}

// SetAttachmentDisposition writes a Content-Disposition header that keeps the
// caller-supplied name INSIDE the filename parameter.
//
// Four handlers built this header by concatenating a name into a quoted string
// (ticket attachments, both file-manager download paths, the since removed
// Library) and one
// of them did not even quote it. A name is not a token: a space ends an
// unquoted one, and a quote closes a quoted one, so `report".pdf` produced
//
//	attachment; filename="report".pdf"
//
// which lets the uploader append their own parameters. RFC 6266 gives
// filename* precedence over filename, so a crafted name decides what the
// DOWNLOADER's browser saves the file as - and on a ticket attachment the
// uploader and the downloader are different people.
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
func SetAttachmentDisposition(w http.ResponseWriter, filename string) {
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": filename}); v != "" {
		w.Header().Set("Content-Disposition", v)
		return
	}
	w.Header().Set("Content-Disposition", "attachment")
}

// ParseIntDefault parses v, or returns def when v is empty or not a number.
func ParseIntDefault(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func MustAtoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// SendCooldown answers a caller while the upstream is cooling down.
func SendCooldown(w http.ResponseWriter, upstream string, left time.Duration) {
	secs := int(left.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	SendJSONError(w, upstream+" is rate limiting requests, try again in "+strconv.Itoa(secs)+" seconds", http.StatusServiceUnavailable)
}

// UsagePeriod resolves the billing month from an optional ?period=YYYY-MM query,
// defaulting to the current UTC month (the aggregator's key).
func UsagePeriod(r *http.Request) time.Time {
	if p := r.URL.Query().Get("period"); p != "" {
		if t, err := time.Parse("2006-01", p); err == nil {
			return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
	}
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}
