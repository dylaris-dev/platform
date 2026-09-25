package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dylaris-core/models"

	"github.com/gorilla/mux"
)

type TicketAttachmentsHandler struct {
	state   *AppState
	scanner AttachmentScanner
}

func NewTicketAttachmentsHandler(state *AppState) *TicketAttachmentsHandler {
	return &TicketAttachmentsHandler{
		state:   state,
		scanner: newAttachmentScanner(),
	}
}

// allowedAttachmentExts maps an accepted lower-case file extension to the
// validation family attachmentAllowed applies to it:
//   - "image/", "application/pdf", "application/zip" - tight families: the
//     http.DetectContentType sniff of the real bytes must start with exactly
//     that string.
//   - "text" - the sniff must start with "text/" and must NOT be "text/html"
//     (an HTML/script body is not what a .txt/.log/.json extension means).
//   - "gzip" - the peeked head must start with the real gzip magic
//     (\x1F\x8B), checked on the raw bytes rather than the sniff string.
//   - "tar" - the peeked head must carry the real POSIX ustar magic
//     ("ustar") at its fixed offset (257), checked on the raw bytes. A sniff
//     family check alone cannot do this job: Go's DetectContentType has no
//     tar signature, so a genuine tar entry AND an ELF/PE payload both sniff
//     as the same application/octet-stream - only the actual header magic
//     tells them apart.
var allowedAttachmentExts = map[string]string{
	".png":  "image/",
	".jpg":  "image/",
	".jpeg": "image/",
	".gif":  "image/",
	".webp": "image/",
	".pdf":  "application/pdf",
	".txt":  "text",
	".log":  "text",
	".zip":  "application/zip",
	".gz":   "gzip",
	".tar":  "tar",
	".json": "text",
}

// attachmentAllowed enforces the extension allowlist AND that the sniffed
// bytes actually belong to the family that extension requires (defence
// against a .png that is actually something else). It returns the sniffed
// content type on success so the caller can persist and later serve THAT
// value rather than the client's claim.
//
// The client's declared MIME (the multipart part's Content-Type header) is
// deliberately not a parameter here and is not consulted anywhere in this
// file: browsers and upload clients routinely send an inaccurate
// Content-Type (e.g. plain "application/octet-stream" for a real PNG), so
// trusting - or even cross-checking against - that value would either
// falsely reject legitimate uploads or let an attacker's own claim stand in
// as ground truth. Only the extension and http.DetectContentType's sniff of
// the actual leading bytes decide the outcome.
func attachmentAllowed(filename string, head []byte) (string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	family, ok := allowedAttachmentExts[ext]
	if !ok {
		return "", fmt.Errorf("file type %q is not allowed", ext)
	}
	sniff := http.DetectContentType(head) // e.g. "image/png" or "text/plain; charset=utf-8"

	switch family {
	case "text":
		// text/* only - the previous application/octet-stream escape hatch
		// let a raw ELF/PE/Mach-O payload named payload.txt/.log/.json
		// through, since Go's sniffer has no executable signature and falls
		// back to octet-stream for arbitrary binary content. text/html is
		// excluded too, so an HTML/script body uploaded as notes.txt is
		// rejected rather than stored and later served back to a browser.
		if !strings.HasPrefix(sniff, "text/") || strings.HasPrefix(sniff, "text/html") {
			return "", fmt.Errorf("content does not match the %s file type", ext)
		}
		return sniff, nil
	case "gzip":
		// Real gzip magic on the raw bytes, not a loose sniff-string check -
		// the previous "application/* or text/plain" rule accepted a shell
		// script as payload.gz.
		if len(head) < 2 || head[0] != 0x1F || head[1] != 0x8B {
			return "", fmt.Errorf("content does not match the %s file type", ext)
		}
		return sniff, nil
	case "tar":
		// The ustar magic sits at a fixed 257-byte offset within the first
		// (and, for any archive whose first entry uses a full POSIX/GNU
		// header block, only) 512-byte block - well within our 512-byte
		// peek. A pre-POSIX (legacy V7) tar without this magic, or anything
		// shorter than the offset, is rejected: correctness of the check
		// (catching a disguised ELF/PE) takes priority over accepting every
		// historical tar variant.
		const ustarOffset = 257
		if len(head) < ustarOffset+5 || string(head[ustarOffset:ustarOffset+5]) != "ustar" {
			return "", fmt.Errorf("content does not match the %s file type", ext)
		}
		return sniff, nil
	default:
		// Tight families: image/*, application/pdf, application/zip.
		if strings.HasPrefix(sniff, family) {
			return sniff, nil
		}
		return "", fmt.Errorf("content does not match the declared %s type", ext)
	}
}

// clampAttachmentContentType returns mime unchanged when it is safe to
// reflect verbatim on a download response, otherwise the deliberately inert
// "application/octet-stream" fallback. Mirrors the sniff families
// attachmentAllowed accepts at upload time (image/*, application/pdf,
// application/zip, text/* other than text/html, and the
// application/octet-stream / application/gzip / application/x-tar shapes a
// valid gzip/tar/JSON payload sniffs as) so this only ever narrows an
// existing row's type, never widens it.
//
// This exists for rows written BEFORE the sniff-based Mime was enforced
// (Task 7): those can still hold the client's originally DECLARED
// Content-Type - e.g. "text/html" - since only the extension and sniffed
// bytes decide storage now, but nothing retroactively fixed already-stored
// rows. nosniff plus Content-Disposition: attachment already stop a browser
// from acting on the BODY; this closes the same gap for the Content-Type
// header itself, for old and new rows alike, without a data migration.
func clampAttachmentContentType(mime string) string {
	base := mime
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	switch {
	case strings.HasPrefix(base, "text/html"):
		return "application/octet-stream"
	case strings.HasPrefix(base, "image/"),
		strings.HasPrefix(base, "text/"),
		base == "application/pdf",
		base == "application/zip",
		base == "application/gzip",
		base == "application/x-gzip",
		base == "application/x-tar",
		base == "application/octet-stream":
		return mime
	default:
		return "application/octet-stream"
	}
}

// AttachmentScanner is the optional AV hook. A nil scanner means no scanning
// (default). A hit (or any scan error - see newAttachmentScanner) returns a
// non-nil error. A genuine virus hit wraps errScanHit (via errors.Is); any
// other error signals an infrastructure failure (dial/read/write/malformed
// reply) - UploadAttachment rejects the upload in both cases (fail-closed)
// but returns a different HTTP status for each.
type AttachmentScanner interface {
	Scan(r io.Reader) error
}

// errScanHit is the sentinel a scanner wraps into its returned error when it
// reports an actual virus hit (as opposed to being unable to complete the
// scan at all). Used with errors.Is so UploadAttachment can return 415 for a
// real hit and 503 for a scanner/infrastructure problem, while still
// rejecting the upload in both cases.
var errScanHit = errors.New("attachment rejected by virus scan")

// newAttachmentScanner returns a clamd INSTREAM scanner when CLAMAV_ADDR is
// set, otherwise nil (scanning off by default; enabling it is a config
// flip, no code change). When nil, UploadAttachment skips the scan step
// entirely: no dial attempt, no added latency, no failure mode.
func newAttachmentScanner() AttachmentScanner {
	addr := strings.TrimSpace(os.Getenv("CLAMAV_ADDR"))
	if addr == "" {
		return nil
	}
	return &clamdScanner{addr: addr}
}

// clamdScanner streams an upload to clamd via the INSTREAM command and
// rejects on a virus hit. Kept small and dependency-free (raw TCP INSTREAM
// framing, no clamd client library).
//
// Fail-closed by design: Scan returns a non-nil error both on an actual
// virus hit AND on any infrastructure failure (dial timeout, write/read
// error, malformed reply). UploadAttachment treats every non-nil error from
// Scan identically - reject the upload. An operator who deliberately set
// CLAMAV_ADDR opted into scanning; if clamd is down we must not silently let
// unscanned uploads through.
type clamdScanner struct{ addr string }

func (c *clamdScanner) Scan(r io.Reader) error {
	conn, err := net.DialTimeout("tcp", c.addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("clamav dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return fmt.Errorf("clamav write cmd: %w", err)
	}
	buf := make([]byte, 32*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			var sz [4]byte
			sz[0] = byte(n >> 24)
			sz[1] = byte(n >> 16)
			sz[2] = byte(n >> 8)
			sz[3] = byte(n)
			if _, err := conn.Write(sz[:]); err != nil {
				return fmt.Errorf("clamav chunk size: %w", err)
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return fmt.Errorf("clamav chunk: %w", err)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("clamav read source: %w", rerr)
		}
	}
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil { // zero-length ends stream
		return fmt.Errorf("clamav end stream: %w", err)
	}
	resp := make([]byte, 512)
	n, _ := conn.Read(resp)
	out := string(resp[:n])
	if strings.Contains(out, "FOUND") {
		return fmt.Errorf("%w: %s", errScanHit, strings.TrimSpace(out))
	}
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────

// canAttach gates upload + delete. Mirrors canReply from tickets.go but
// without the "user reply → may not internal" carve-out — internal-ness
// belongs to the message, not the attachment.
func (h *TicketAttachmentsHandler) canAttach(t *models.Ticket, perms EffectivePermissions, userID string, isWatcher, watcherCanReply bool) bool {
	// Uploading and deleting are acting, so they follow tickets.write. Reading
	// an attachment stays with IsSupport, beside reading the ticket it hangs on.
	if perms.IsAdmin || perms.CanManageTickets {
		return true
	}
	if t.UserID == userID {
		return true
	}
	if isWatcher && watcherCanReply {
		return true
	}
	return false
}

func (h *TicketAttachmentsHandler) loadTicketAndGate(w http.ResponseWriter, r *http.Request) (*models.Ticket, EffectivePermissions, string, bool) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		sendJSONError(w, "Invalid ticket id", http.StatusBadRequest)
		return nil, EffectivePermissions{}, "", false
	}
	t, err := h.state.Store.GetTicket(id)
	if err != nil || t == nil {
		sendJSONError(w, "Ticket not found", http.StatusNotFound)
		return nil, EffectivePermissions{}, "", false
	}
	userID, _ := r.Context().Value("userID").(string)
	perms := LoadEffectivePermissions(h.state, userID)
	return t, perms, userID, true
}

// randomAttachmentID returns 16 hex chars. Used as part of the storage key
// so two uploads with the same filename don't collide.
func randomAttachmentID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sanitizeAttachmentFilename strips path separators and trims to a sensible
// length. Kept separate from the file-handler's sanitizeFilename helper to
// avoid coupling two unrelated subsystems through a shared identifier.
func sanitizeAttachmentFilename(in string) string {
	in = strings.TrimSpace(in)
	in = strings.ReplaceAll(in, "/", "_")
	in = strings.ReplaceAll(in, "\\", "_")
	in = strings.ReplaceAll(in, "..", "_")
	if len(in) > 200 {
		in = in[:200]
	}
	if in == "" {
		in = "file"
	}
	return in
}

// attachmentUploadBody peeks the first 512 bytes of file for content-type
// sniffing, then rewinds file back to the start so the FULL, untouched file
// (start to finish) is what streams to storage - the sniff peek must never
// consume bytes from what actually gets stored.
//
// body is file itself, not a wrapper: multipart.File is guaranteed to
// implement io.Seeker, so rewinding keeps body just as seekable as file was
// before the peek. Returning anything else here - notably an io.MultiReader
// stitching head back together with the remainder of file - reintroduces the
// bug this helper exists to prevent: S3Provider.WriteFile
// (core/storage/s3provider.go) passes body straight through to the AWS SDK's
// PutObject with no content length, and the SDK's checksum middleware
// refuses an unseekable stream outright on a plain-HTTP endpoint - exactly
// the documented self-hosted MinIO use case (http://minio:9000) - so every
// attachment upload 500'd on such an install until this rewind-and-reuse fix
// landed ("compute input header checksum failed, unseekable stream is not
// supported without TLS and trailing checksum").
func attachmentUploadBody(file multipart.File) (body io.Reader, head []byte, err error) {
	buf := make([]byte, 512)
	n, rerr := io.ReadFull(file, buf)
	if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
		// A short file (fewer than 512 bytes) legitimately yields EOF or
		// ErrUnexpectedEOF - that's fine, n still holds however many bytes
		// were read. Any other error is a genuine read failure; continuing
		// would sniff an empty/truncated head as text/plain and could let
		// content through on zero observed bytes.
		return nil, nil, fmt.Errorf("attachment upload peek read: %w", rerr)
	}
	if _, serr := file.Seek(0, io.SeekStart); serr != nil {
		return nil, nil, fmt.Errorf("attachment upload seek to start: %w", serr)
	}
	return file, buf[:n], nil
}

// ── Endpoints ────────────────────────────────────────────────────────

// UploadAttachment POST /api/tickets/{id}/attachments
// Multipart with field name "file". Optional form field "messageId" links
// the attachment to a specific message (useful when posting a new reply
// with attachments — frontend posts message first, then attaches by id).
const (
	// ticketUploadHardCapMB applies when the per-file limit is 0 ("unlimited").
	// Unlimited is about what an operator wants to ALLOW, not an invitation to
	// write unbounded data to Core's disk before any check runs. 1024 is the same
	// ceiling LoadTicketSettings already refuses to read a larger value than.
	ticketUploadHardCapMB = 1024

	// ticketUploadEnvelopeSlack covers the multipart boundaries, part headers and
	// the small messageId field, so a file of EXACTLY the per-file limit still
	// gets through to the quota check that is meant to judge it.
	ticketUploadEnvelopeSlack = 1 << 20
)

// ticketUploadBodyLimit converts the per-file setting into a request-body cap.
func ticketUploadBodyLimit(maxFileMB *int64) int64 {
	// No cap configured, or one above the hard cap, both land on the hard cap:
	// this bounds what the SERVER will read, and "the admin set no limit" cannot
	// mean "read an unbounded body".
	//
	// A cap of 0 is different and is honoured as 0 - attachments are not allowed,
	// so nothing needs reading. The envelope slack still rides along, because the
	// multipart framing is not the file and refusing it before the handler can
	// name the reason would surface as a hang rather than a message.
	mb := int64(ticketUploadHardCapMB)
	if maxFileMB != nil && *maxFileMB >= 0 && *maxFileMB < ticketUploadHardCapMB {
		mb = *maxFileMB
	}
	return mb*1024*1024 + ticketUploadEnvelopeSlack
}

// UploadAttachment POST /api/tickets/{id}/attachments - attaches a file to a
// ticket. A watcher needs the reply flag to upload. The type is decided by
// sniffing the content, not by what the client declared.
func (h *TicketAttachmentsHandler) UploadAttachment(w http.ResponseWriter, r *http.Request) {
	t, perms, userID, ok := h.loadTicketAndGate(w, r)
	if !ok {
		return
	}
	isWatcher, _ := h.state.Store.IsTicketWatcher(t.ID, userID)
	watcherCanReply := false
	if isWatcher {
		watchers, _ := h.state.Store.ListTicketWatchers(t.ID)
		for _, w := range watchers {
			if w.UserID == userID {
				watcherCanReply = w.CanReply
				break
			}
		}
	}
	if !h.canAttach(t, perms, userID, isWatcher, watcherCanReply) {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}

	settings := LoadTicketSettings(h.state)

	// Bound the body BEFORE parsing it. ParseMultipartForm reads the WHOLE
	// request first - spilling everything past its memory budget to a temp file -
	// and only then do the per-file / per-ticket / per-user quota checks below
	// run. Without a cap, a caller holding attach rights could write an
	// arbitrarily large file to Core's disk and be told "too large" afterwards,
	// which is the wrong order for a limit to be applied in.
	//
	// LimitBody's own comment already assumed this was handled - "the upload
	// handlers set their own, much larger MaxBytesReader" - and the server file
	// upload does exactly that. This handler did not.
	// capBody, not a bare MaxBytesReader: an over-limit upload has to be refused
	// before the first body read or a client using Expect: 100-continue hangs
	// instead of being told (see capBody).
	if !capBody(w, r, ticketUploadBodyLimit(settings.MaxFileSizeMB)) {
		return
	}

	// Multipart parse with a generous max-memory; the file is streamed to
	// disk regardless.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			sendJSONError(w, "Upload is too large", http.StatusRequestEntityTooLarge)
			return
		}
		sendJSONError(w, "Failed to parse upload", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		sendJSONError(w, "Missing 'file' field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// The multipart part's declared Content-Type header is deliberately never
	// read here: it is neither validated against nor persisted (see the
	// attachmentAllowed doc comment) - only the extension and the sniffed
	// bytes decide what gets accepted and what gets stored as the Mime.
	filename := sanitizeAttachmentFilename(header.Filename)
	size := header.Size

	// Quota checks before writing.
	// Each of these three guarded on `> 0`, so a cap of 0 - the admin saying
	// attachments are not allowed - disabled the very check it was setting.
	if settings.MaxFileSizeMB != nil && size > *settings.MaxFileSizeMB*1024*1024 {
		sendJSONError(w, fmt.Sprintf("File exceeds the %d MB per-file limit", *settings.MaxFileSizeMB), http.StatusRequestEntityTooLarge)
		return
	}
	if settings.MaxTicketSizeMB != nil {
		maxTicket := *settings.MaxTicketSizeMB * 1024 * 1024
		// A quota that cannot be read must not read as empty: discarding this
		// error made `current` 0, so only the per-FILE limit still applied and
		// the per-ticket total could be exceeded freely.
		current, cerr := h.state.Store.SumAttachmentBytesByTicket(t.ID)
		if cerr != nil {
			sendJSONError(w, "Could not verify the ticket attachment quota", http.StatusInternalServerError)
			return
		}
		if current+size > maxTicket {
			sendJSONError(w, fmt.Sprintf("Adding this file would exceed the %d MB per-ticket limit", *settings.MaxTicketSizeMB), http.StatusRequestEntityTooLarge)
			return
		}
	}
	if settings.MaxUserSizeMB != nil {
		maxUser := *settings.MaxUserSizeMB * 1024 * 1024
		current, cerr := h.state.Store.SumAttachmentBytesByUser(userID)
		if cerr != nil {
			sendJSONError(w, "Could not verify your attachment quota", http.StatusInternalServerError)
			return
		}
		if current+size > maxUser {
			sendJSONError(w, fmt.Sprintf("Adding this file would exceed your %d MB attachment quota", *settings.MaxUserSizeMB), http.StatusRequestEntityTooLarge)
			return
		}
	}

	// Peek the first bytes for a content sniff, then rewind so the full file
	// still streams to storage untouched - see attachmentUploadBody's doc
	// comment for why body must stay a rewound file, never an io.MultiReader
	// stitched from head + file.
	body, head, err := attachmentUploadBody(file)
	if err != nil {
		log.Printf("ticket-attachments: %v (ticket %d)", err, t.ID)
		sendJSONError(w, "Failed to read upload", http.StatusInternalServerError)
		return
	}
	sniffedMime, err := attachmentAllowed(filename, head)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusUnsupportedMediaType)
		return
	}

	// Optional AV scan: when a scanner is configured, spool to a temp file so
	// the whole object can be scanned before it lands in storage. Disabled
	// (h.scanner == nil) by default: no temp file, no dial, no latency.
	if h.scanner != nil {
		tmp, err := os.CreateTemp("", "dylaris-att-*")
		if err != nil {
			sendJSONError(w, "Failed to buffer upload", http.StatusInternalServerError)
			return
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		if _, err := io.Copy(tmp, body); err != nil {
			sendJSONError(w, "Failed to buffer upload", http.StatusInternalServerError)
			return
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			sendJSONError(w, "Failed to re-read upload", http.StatusInternalServerError)
			return
		}
		// Fail-closed: any non-nil error from Scan (a genuine hit OR an
		// infrastructure failure such as clamd being unreachable) rejects the
		// upload either way. See the clamdScanner doc comment for the
		// rationale. The HTTP status and message differ though: a real hit is
		// a client-content problem (415), an infra failure is a server-side
		// condition (503) - and the raw error (which can contain internal
		// host/port details) is logged server-side only, never returned to
		// the ticket user.
		if serr := h.scanner.Scan(tmp); serr != nil {
			if errors.Is(serr, errScanHit) {
				sendJSONError(w, "File rejected by virus scan", http.StatusUnsupportedMediaType)
			} else {
				log.Printf("ticket-attachments: scan failed for ticket %d: %v", t.ID, serr)
				sendJSONError(w, "Attachment scanning is temporarily unavailable, please try again later", http.StatusServiceUnavailable)
			}
			return
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			sendJSONError(w, "Failed to re-read upload", http.StatusInternalServerError)
			return
		}
		body = tmp // stream the scanned temp file to storage
	}

	prov, err := h.state.buildCoreStorageProvider(CoreStoragePrefixAttachments)
	if err != nil {
		coreStorageUnavailableResponse(w, err)
		return
	}

	// Storage key: tickets/<id>/<random>-<filename>. Avoids collisions and
	// keeps per-ticket pruning trivial when the ticket gets hard-deleted.
	attachID := randomAttachmentID()
	storageKey := fmt.Sprintf("tickets/%d/%s-%s", t.ID, attachID, filename)
	if err := prov.WriteFile(r.Context(), storageKey, body); err != nil {
		sendJSONError(w, "Failed to store file", http.StatusInternalServerError)
		return
	}

	// Optional message link from form.
	var msgID *int
	if v := strings.TrimSpace(r.FormValue("messageId")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			msgID = &n
		}
	}

	uid := userID
	a := &models.TicketAttachment{
		TicketID:   t.ID,
		MessageID:  msgID,
		Filename:   filename,
		Mime:       sniffedMime,
		SizeBytes:  size,
		StorageKey: storageKey,
		UploadedBy: &uid,
	}
	insertedID, err := h.state.Store.AddTicketAttachment(a)
	if err != nil {
		// Best-effort cleanup if the DB rejected after the file landed.
		_ = prov.DeletePath(r.Context(), storageKey)
		sendJSONError(w, "Failed to persist metadata", http.StatusInternalServerError)
		return
	}
	a.ID = insertedID

	aid := userID
	_ = h.state.Store.InsertTicketAudit(&models.TicketAuditEvent{
		TicketID:    t.ID,
		EventType:   TicketEventAttachmentAdded,
		ActorUserID: &aid,
		Metadata: map[string]interface{}{
			"filename": filename,
			"size":     size,
		},
	})

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"attachment": a,
	})
}

// ListAttachments GET /api/tickets/{id}/attachments - the files on one ticket,
// once the caller passes the same visibility check the ticket itself uses:
// author, watcher, or staff whose support team matches.
func (h *TicketAttachmentsHandler) ListAttachments(w http.ResponseWriter, r *http.Request) {
	t, perms, userID, ok := h.loadTicketAndGate(w, r)
	if !ok {
		return
	}
	settings := LoadTicketSettings(h.state)
	isWatcher, _ := h.state.Store.IsTicketWatcher(t.ID, userID)
	myTeam := ""
	if me, err := h.state.Store.GetUserByID(userID); err == nil && me != nil {
		myTeam = me.SupportTeam
	}
	if !canSeeTicket(t, perms, userID, isWatcher, settings, myTeam) {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	atts, err := h.state.Store.ListTicketAttachments(t.ID)
	if err != nil {
		sendJSONError(w, "Failed to load attachments", http.StatusInternalServerError)
		return
	}
	if atts == nil {
		atts = []models.TicketAttachment{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"attachments": atts,
	})
}

// DownloadAttachment GET /api/tickets/{id}/attachments/{aid}/download -
// streams one attachment. The stored MIME type is clamped and sent with
// nosniff and an attachment disposition, so an old row holding a client-
// declared text/html cannot render in the browser.
func (h *TicketAttachmentsHandler) DownloadAttachment(w http.ResponseWriter, r *http.Request) {
	t, perms, userID, ok := h.loadTicketAndGate(w, r)
	if !ok {
		return
	}
	settings := LoadTicketSettings(h.state)
	isWatcher, _ := h.state.Store.IsTicketWatcher(t.ID, userID)
	myTeam := ""
	if me, err := h.state.Store.GetUserByID(userID); err == nil && me != nil {
		myTeam = me.SupportTeam
	}
	if !canSeeTicket(t, perms, userID, isWatcher, settings, myTeam) {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	aid, err := strconv.Atoi(mux.Vars(r)["aid"])
	if err != nil || aid <= 0 {
		sendJSONError(w, "Invalid attachment id", http.StatusBadRequest)
		return
	}
	a, err := h.state.Store.GetTicketAttachment(aid)
	if err != nil || a == nil || a.TicketID != t.ID {
		sendJSONError(w, "Attachment not found", http.StatusNotFound)
		return
	}

	prov, err := h.state.buildCoreStorageProvider(CoreStoragePrefixAttachments)
	if err != nil {
		coreStorageUnavailableResponse(w, err)
		return
	}

	// Deliberately never tries provider.DownloadURL, unlike the library and
	// ticket-backups download handlers: a pre-signed S3 redirect would let
	// the object storage backend serve its own headers, bypassing the
	// nosniff / exact Content-Length / Content-Disposition enforcement below.
	// Attachments always stream through Core so those headers are guaranteed,
	// at the cost of not offloading the transfer to the object store.
	rc, err := prov.GetFile(r.Context(), a.StorageKey)
	if err != nil {
		sendJSONError(w, "File missing on storage", http.StatusGone)
		return
	}
	defer rc.Close()

	// The stored Mime is the sniffed type from upload time (never the
	// client's declared claim - see attachmentAllowed), and nosniff plus
	// Content-Disposition: attachment together stop a browser from executing
	// or rendering the body regardless of what Content-Type it carries.
	// clampAttachmentContentType additionally covers rows written before that
	// sniff-based Mime was enforced, which can still hold a client-declared
	// value like text/html - see its doc comment.
	w.Header().Set("Content-Type", clampAttachmentContentType(a.Mime))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", a.SizeBytes))
	setAttachmentDisposition(w, a.Filename)
	io.Copy(w, rc)
}

// DeleteAttachment DELETE /api/tickets/{id}/attachments/{aid}
// Uploader OR support/admin OR ticket owner can delete.
func (h *TicketAttachmentsHandler) DeleteAttachment(w http.ResponseWriter, r *http.Request) {
	t, perms, userID, ok := h.loadTicketAndGate(w, r)
	if !ok {
		return
	}
	aid, _ := strconv.Atoi(mux.Vars(r)["aid"])
	a, err := h.state.Store.GetTicketAttachment(aid)
	if err != nil || a == nil || a.TicketID != t.ID {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	allowed := perms.IsAdmin || perms.IsSupport || t.UserID == userID
	if !allowed && a.UploadedBy != nil && *a.UploadedBy == userID {
		allowed = true
	}
	if !allowed {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	if err := h.state.Store.DeleteTicketAttachment(aid); err != nil {
		sendJSONError(w, "Delete failed", http.StatusInternalServerError)
		return
	}
	// Best-effort blob cleanup: the DB row is already gone, so storage being
	// unavailable here must not fail the whole request - it's logged and
	// skipped, exactly like TicketDeletionsHandler.DeleteTicket's cascade
	// cleanup (ticket_deletions.go).
	if prov, perr := h.state.buildCoreStorageProvider(CoreStoragePrefixAttachments); perr != nil {
		log.Printf("ticket-attachments: core storage unavailable, skipping blob cleanup for attachment %d (%s): %v", aid, a.StorageKey, perr)
	} else {
		_ = prov.DeletePath(r.Context(), a.StorageKey)
	}
	_ = h.state.Store.InsertTicketAudit(&models.TicketAuditEvent{
		TicketID:    t.ID,
		EventType:   TicketEventAttachmentRemoved,
		ActorUserID: &userID,
		Metadata: map[string]interface{}{
			"filename": a.Filename,
		},
	})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
