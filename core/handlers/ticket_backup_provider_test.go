package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/storage"
	"dylaris-core/store"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gorilla/mux"
	"github.com/pquerna/otp/totp"
)

// memProvider is a minimal in-memory StorageProvider fixture used to pin the
// provider contract the backup handlers are rewritten against. Local to this
// file; never redeclared elsewhere.
type memProvider struct{ m map[string][]byte }

func newMemProvider() *memProvider { return &memProvider{m: map[string][]byte{}} }

func (p *memProvider) ListFiles(_ context.Context, path string) ([]storage.FileInfo, error) {
	var out []storage.FileInfo
	for k, v := range p.m {
		out = append(out, storage.FileInfo{Name: k, Size: int64(len(v)), Enabled: true})
	}
	return out, nil
}
func (p *memProvider) GetFile(_ context.Context, path string) (io.ReadCloser, error) {
	b, ok := p.m[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}
func (p *memProvider) DeletePath(_ context.Context, path string) error {
	delete(p.m, path)
	return nil
}
func (p *memProvider) CreateDir(context.Context, string) error           { return nil }
func (p *memProvider) CopyToLocal(context.Context, string, string) error { return nil }
func (p *memProvider) WriteFile(_ context.Context, path string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	p.m[path] = b
	return nil
}
func (p *memProvider) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func (p *memProvider) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func TestBackupProvider_WriteListReadDelete(t *testing.T) {
	p := newMemProvider()
	if err := p.WriteFile(context.Background(), "tickets-20260718-101010.json", strings.NewReader(`{"ok":true}`)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	files, _ := p.ListFiles(context.Background(), "/")
	if len(files) != 1 || files[0].Name != "tickets-20260718-101010.json" {
		t.Fatalf("ListFiles = %+v, want the one backup", files)
	}
	rc, err := p.GetFile(context.Background(), "tickets-20260718-101010.json")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != `{"ok":true}` {
		t.Errorf("GetFile = %q", got)
	}
	if err := p.DeletePath(context.Background(), "tickets-20260718-101010.json"); err != nil {
		t.Fatalf("DeletePath: %v", err)
	}
	if files, _ := p.ListFiles(context.Background(), "/"); len(files) != 0 {
		t.Errorf("backup still present after delete: %+v", files)
	}
}

// ── ticketBackupFakeStore: the small store surface CreateBackup/InitRestore/
// ExecuteRestore/buildCoreStorageProvider touch ──

// ticketBackupFakeStore covers exactly the store.Store methods the backup +
// restore handlers call: DumpTicketTable (CreateBackup), GetUserByID
// (InitRestore/ExecuteRestore's 2FA gate), RawDB (wipeAndReload's raw SQL
// access), InsertAuditIdentity (the audit log on a successful restore) and
// GetSetting (buildCoreStorageProvider's per-request config resolution,
// added when Change 3 removed TicketMigrationHandler's cached provider
// field). Distinct from every other fake in this package.
type ticketBackupFakeStore struct {
	store.Store
	user       *models.User
	db         *sql.DB
	dumpErr    error
	auditCalls int
	settings   map[string]string
}

func (f *ticketBackupFakeStore) DumpTicketTable(table string) ([]map[string]interface{}, error) {
	if f.dumpErr != nil {
		return nil, f.dumpErr
	}
	return nil, nil
}

func (f *ticketBackupFakeStore) GetUserByID(id string) (*models.User, error) {
	if f.user != nil && f.user.ID == id {
		return f.user, nil
	}
	return nil, nil
}

func (f *ticketBackupFakeStore) RawDB() *sql.DB { return f.db }

func (f *ticketBackupFakeStore) InsertAuditIdentity(ev *models.AuditEventIdentity) error {
	f.auditCalls++
	return nil
}

func (f *ticketBackupFakeStore) GetSetting(key string) (string, error) {
	return f.settings[key], nil
}

// newTicketBackupHandler builds a TicketMigrationHandler wired against a REAL
// Core file storage "path" backend rooted at a fresh, real, absolute-looking
// test directory (see testConnectionProbeDir, core_storage_http_test.go), so
// CreateBackup/ListBackups/DownloadBackup/DeleteBackup/InitRestore/
// ExecuteRestore resolve a genuine *storage.LocalProvider per request exactly
// like production. Change 3 removed the injectable provider field the old
// version of this helper set directly via a fake ticketBackupMemProvider.
//
// Returns the handler and the ticket-backups-SCOPED directory
// (dir/ticket-backups) so callers can seed/read backup files directly on
// disk.
func newTicketBackupHandler(t *testing.T, fs *ticketBackupFakeStore) (*TicketMigrationHandler, string) {
	t.Helper()
	dir := testConnectionProbeDir(t)
	if fs.settings == nil {
		fs.settings = map[string]string{}
	}
	fs.settings[keyCoreStorageBackend] = "path"
	fs.settings[keyCoreStoragePath] = dir
	fs.settings[keyCoreStoragePathConfirm] = "true"
	h := &TicketMigrationHandler{
		state:  &AppState{Store: fs},
		tokens: make(map[string]*restoreToken),
	}
	return h, filepath.Join(dir, CoreStoragePrefixBackups)
}

// ── Round trip: create -> list -> download -> restore ──

// TestTicketBackups_CreateListDownloadRestore_RoundTrip is the central
// non-regression test for this task: every backup operation must now flow
// through the shared storage.StorageProvider, and a backup created that way
// must still be listable, downloadable byte-for-byte, and restorable end to
// end (2FA + cooldown + confirmation phrase gates all still enforced).
func TestTicketBackups_CreateListDownloadRestore_RoundTrip(t *testing.T) {
	secret := freshTOTPSecret(t) // reused from totp_verify_test.go
	user := &models.User{ID: "admin-1", Is2FAEnabled: true, TOTPSecret: secret}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	fs := &ticketBackupFakeStore{user: user, db: db}
	h, backupsDir := newTicketBackupHandler(t, fs)

	// 1) Create.
	createRW := httptest.NewRecorder()
	h.CreateBackup(createRW, adminReq(httptest.NewRequest(http.MethodPost, "/api/admin/tickets/backup", nil)))
	if createRW.Code != http.StatusOK {
		t.Fatalf("CreateBackup status = %d, want 200 (%s)", createRW.Code, createRW.Body.String())
	}
	var createResp struct {
		Success bool          `json:"success"`
		Backup  backupSummary `json:"backup"`
	}
	if err := json.Unmarshal(createRW.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("decode CreateBackup response: %v", err)
	}
	name := createResp.Backup.Name
	if name == "" {
		t.Fatal("CreateBackup returned no backup name")
	}
	if !strings.HasPrefix(name, "tickets-") || !strings.HasSuffix(name, ".json") {
		t.Fatalf("unexpected backup name shape: %q", name)
	}

	wantBody, err := os.ReadFile(filepath.Join(backupsDir, name))
	if err != nil {
		t.Fatalf("CreateBackup did not write %q to disk: %v", name, err)
	}
	if len(wantBody) == 0 {
		t.Fatalf("CreateBackup wrote an empty file under %q", name)
	}

	// 2) List.
	listRW := httptest.NewRecorder()
	h.ListBackups(listRW, adminReq(httptest.NewRequest(http.MethodGet, "/api/admin/tickets/backups", nil)))
	if listRW.Code != http.StatusOK {
		t.Fatalf("ListBackups status = %d, want 200 (%s)", listRW.Code, listRW.Body.String())
	}
	var listResp struct {
		Success bool            `json:"success"`
		Backups []backupSummary `json:"backups"`
	}
	if err := json.Unmarshal(listRW.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode ListBackups response: %v", err)
	}
	if len(listResp.Backups) != 1 || listResp.Backups[0].Name != name {
		t.Fatalf("ListBackups = %+v, want exactly the just-created backup %q", listResp.Backups, name)
	}

	// 3) Download - the path backend returns ("", nil) from DownloadURL, so
	// this must stream, not redirect, and the streamed bytes must equal what
	// was written to disk.
	dlReq := adminReq(httptest.NewRequest(http.MethodGet, "/api/admin/tickets/backups/"+name+"/download", nil))
	dlReq = mux.SetURLVars(dlReq, map[string]string{"name": name})
	dlRW := httptest.NewRecorder()
	h.DownloadBackup(dlRW, dlReq)
	if dlRW.Code != http.StatusOK {
		t.Fatalf("DownloadBackup status = %d, want 200 (%s)", dlRW.Code, dlRW.Body.String())
	}
	if !bytes.Equal(dlRW.Body.Bytes(), wantBody) {
		t.Fatalf("downloaded body does not match what was written to disk")
	}

	// 4) InitRestore.
	initBody, _ := json.Marshal(restoreInitRequest{Name: name})
	initReq := adminReq(httptest.NewRequest(http.MethodPost, "/api/admin/tickets/restore/init", bytes.NewReader(initBody)))
	initReq = initReq.WithContext(context.WithValue(initReq.Context(), "userID", user.ID))
	initRW := httptest.NewRecorder()
	h.InitRestore(initRW, initReq)
	if initRW.Code != http.StatusOK {
		t.Fatalf("InitRestore status = %d, want 200 (%s)", initRW.Code, initRW.Body.String())
	}
	var initResp struct {
		Success            bool   `json:"success"`
		Token              string `json:"token"`
		ConfirmationPhrase string `json:"confirmationPhrase"`
	}
	if err := json.Unmarshal(initRW.Body.Bytes(), &initResp); err != nil {
		t.Fatalf("decode InitRestore response: %v", err)
	}

	// Bypass the 15s human cooldown deliberately (white-box, same package):
	// it is a UX safety net unrelated to what this task changed. Everything
	// else - token/2FA/phrase checks and the eventual provider read - still
	// runs for real below.
	h.tokensMu.Lock()
	if tok, ok := h.tokens[initResp.Token]; ok {
		tok.MinExecuteAfter = time.Now().Add(-time.Second)
	}
	h.tokensMu.Unlock()

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}

	// wipeAndReload wipes every ticket table (reverse insert order) then
	// reloads from the decoded backup. DumpTicketTable's fake returns zero
	// rows for every table, so the backup's "tables" payload decodes to
	// nil/empty slices and only the DELETE half of wipeAndReload runs.
	mock.ExpectBegin()
	tables := store.TicketTablesInOrder()
	for i := len(tables) - 1; i >= 0; i-- {
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM " + tables[i])).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectCommit()

	execBody, _ := json.Marshal(restoreExecuteRequest{
		Token:              initResp.Token,
		TOTPCode:           code,
		ConfirmationPhrase: initResp.ConfirmationPhrase,
	})
	execReq := adminReq(httptest.NewRequest(http.MethodPost, "/api/admin/tickets/restore/execute", bytes.NewReader(execBody)))
	execReq = execReq.WithContext(context.WithValue(execReq.Context(), "userID", user.ID))
	execRW := httptest.NewRecorder()
	h.ExecuteRestore(execRW, execReq)
	if execRW.Code != http.StatusOK {
		t.Fatalf("ExecuteRestore status = %d, want 200 (%s)", execRW.Code, execRW.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
	if fs.auditCalls != 1 {
		t.Fatalf("audit calls = %d, want 1 (restore success must be audited)", fs.auditCalls)
	}
}

// ── ListBackups: ordering + provider error surfacing ──

// TestListBackups_OrderingIsChronological verifies the accepted trade-off
// documented on ListBackups: with no per-file CreatedAt available from the
// provider, ordering relies purely on the timestamped filename
// (tickets-YYYYMMDD-HHMMSS.json) sorting lexicographically the same as
// chronologically. Names are deliberately written out of order and span a
// year/month/day boundary so a byte-order bug would show up immediately.
func TestListBackups_OrderingIsChronological(t *testing.T) {
	h, backupsDir := newTicketBackupHandler(t, &ticketBackupFakeStore{})
	if err := os.MkdirAll(backupsDir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", backupsDir, err)
	}
	seed := []string{
		"tickets-20260101-000000.json",
		"tickets-20261231-235959.json",
		"tickets-20260630-120000.json",
	}
	for _, n := range seed {
		if err := os.WriteFile(filepath.Join(backupsDir, n), []byte("{}"), 0644); err != nil {
			t.Fatalf("seed %q: %v", n, err)
		}
	}

	rw := httptest.NewRecorder()
	h.ListBackups(rw, adminReq(httptest.NewRequest(http.MethodGet, "/api/admin/tickets/backups", nil)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	var resp struct {
		Backups []backupSummary `json:"backups"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	want := []string{
		"tickets-20261231-235959.json", // newest
		"tickets-20260630-120000.json",
		"tickets-20260101-000000.json", // oldest
	}
	if len(resp.Backups) != len(want) {
		t.Fatalf("got %d backups, want %d (%+v)", len(resp.Backups), len(want), resp.Backups)
	}
	for i, name := range want {
		if resp.Backups[i].Name != name {
			t.Fatalf("position %d = %q, want %q (full order: %+v)", i, resp.Backups[i].Name, name, resp.Backups)
		}
	}
}

// newTicketBackupS3FailFastHandler builds a TicketMigrationHandler configured
// against an s3 backend whose endpoint is a loopback port nothing listens on
// (127.0.0.1:1 refuses the connection instantly - no DNS lookup, no
// listener), so a provider call fails fast with a REAL network error through
// the actual per-request resolution path (buildCoreStorageProvider ->
// newStorageProviderForConfig -> S3Provider), rather than a hand-injected
// fake. This replaces the "path" backend fault-injection this package used to
// rely on for TestListBackups_ProviderErrorSurfaces /
// TestDeleteBackup_ProviderErrorSurfaces: forcing a "path" backend failure by
// blocking its directory with a regular file does not work on Windows (a
// file-blocked "directory" fails os.MkdirAll/os.ReadDir with
// ERROR_PATH_NOT_FOUND, which Go maps to the same fs.ErrNotExist sentinel as
// a genuinely missing directory - and LocalProvider.ListFiles/DeletePath both
// deliberately treat fs.ErrNotExist as "nothing there yet", so the forced
// failure gets silently absorbed instead of propagating). An unreachable s3
// endpoint has no such ambiguity: List/Delete over HTTP either succeeds or
// returns a hard error, on every OS.
//
// Callers must call disableAWSRetries(t) first so the SDK's default
// 3-attempt retry-with-backoff doesn't turn a sub-millisecond refused-
// connection into a several-second test.
func newTicketBackupS3FailFastHandler(t *testing.T) *TicketMigrationHandler {
	t.Helper()
	fs := &ticketBackupFakeStore{settings: map[string]string{
		keyCoreStorageBackend:     "s3",
		keyCoreStorageS3Endpoint:  "http://127.0.0.1:1",
		keyCoreStorageS3Bucket:    "bucket1",
		keyCoreStorageS3AccessKey: "key1",
		keyCoreStorageS3SecretKey: "secret1",
	}}
	return &TicketMigrationHandler{state: &AppState{Store: fs}, tokens: make(map[string]*restoreToken)}
}

// TestListBackups_ProviderErrorSurfaces pins that a provider failure comes
// back as a clear 500 with a non-empty, success:false body - never a silent
// empty 200 that would make a broken backend look like "no backups exist
// yet". See newTicketBackupS3FailFastHandler's doc comment for why this
// drives the real s3 resolution path instead of a fault-injected fake.
func TestListBackups_ProviderErrorSurfaces(t *testing.T) {
	disableAWSRetries(t)
	h := newTicketBackupS3FailFastHandler(t)

	rw := httptest.NewRecorder()
	h.ListBackups(rw, adminReq(httptest.NewRequest(http.MethodGet, "/api/admin/tickets/backups", nil)))
	if rw.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", rw.Code, rw.Body.String())
	}
	if rw.Body.Len() == 0 {
		t.Fatal("empty response body on a provider error")
	}
	var resp struct {
		Success bool `json:"success"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &resp)
	if resp.Success {
		t.Fatal("success=true on a provider error")
	}
}

// ── DownloadBackup: redirect vs stream, and not-found ──

// TestDownloadBackup_RedirectsWhenProviderSignsURL covers the redirect path:
// a configured s3 backend produces a real (offline-computed) pre-signed URL,
// so the handler must 302 straight to it instead of streaming through Core.
func TestDownloadBackup_RedirectsWhenProviderSignsURL(t *testing.T) {
	fs := &ticketBackupFakeStore{settings: map[string]string{
		keyCoreStorageBackend:     "s3",
		keyCoreStorageS3Bucket:    "my-bucket",
		keyCoreStorageS3AccessKey: "AKIAEXAMPLE",
		keyCoreStorageS3SecretKey: "s3cr3t",
	}}
	h := &TicketMigrationHandler{state: &AppState{Store: fs}, tokens: make(map[string]*restoreToken)}

	req := adminReq(httptest.NewRequest(http.MethodGet, "/api/admin/tickets/backups/tickets-x.json/download", nil))
	req = mux.SetURLVars(req, map[string]string{"name": "tickets-x.json"})
	rw := httptest.NewRecorder()
	h.DownloadBackup(rw, req)

	if rw.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (%s)", rw.Code, rw.Body.String())
	}
	loc := rw.Header().Get("Location")
	if loc == "" {
		t.Fatal("Location header is empty, want a pre-signed URL")
	}
	if !strings.Contains(loc, "my-bucket") {
		t.Errorf("Location = %q, want it to reference the configured bucket", loc)
	}
}

// TestDownloadBackup_StreamsWhenProviderDoesNotSignURL locks in the highest-
// value non-regression from the original task brief: the ("", nil) sentinel
// (path backend/LocalProvider - "no URL, stream it yourself") must stay
// byte-identical to the old code.
//
// NOTE: the original version of this test also covered a second case - a
// genuine DownloadURL ERROR (as opposed to the empty-string sentinel) must
// ALSO fall through to streaming, not be conflated with the sentinel. That
// sub-case relied on a hand-injected provider fake returning a non-nil error
// from DownloadURL; Change 3 removed the injectable provider field, and
// neither a real LocalProvider (DownloadURL always returns ("", nil)) nor a
// real S3Provider offers a deterministic, offline way to force a DownloadURL
// error, so that sub-case is no longer covered here. The handler code for
// this branch (`if url, err := ...; err == nil && url != ""`) is unchanged by
// this refactor.
func TestDownloadBackup_StreamsWhenProviderDoesNotSignURL(t *testing.T) {
	h, backupsDir := newTicketBackupHandler(t, &ticketBackupFakeStore{})
	if err := os.MkdirAll(backupsDir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", backupsDir, err)
	}
	body := []byte(`{"backup_at":"2026-07-18T00:00:00Z"}`)
	if err := os.WriteFile(filepath.Join(backupsDir, "tickets-x.json"), body, 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := adminReq(httptest.NewRequest(http.MethodGet, "/api/admin/tickets/backups/tickets-x.json/download", nil))
	req = mux.SetURLVars(req, map[string]string{"name": "tickets-x.json"})
	rw := httptest.NewRecorder()
	h.DownloadBackup(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stream, not redirect) (%s)", rw.Code, rw.Body.String())
	}
	if loc := rw.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, want empty (no redirect when streaming)", loc)
	}
	if !bytes.Equal(rw.Body.Bytes(), body) {
		t.Errorf("body = %q, want %q", rw.Body.Bytes(), body)
	}
	if ct := rw.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestDownloadBackup_NotFoundWhenMissing pins that a missing key surfaces as
// 404, not a silent 200 with an empty body.
func TestDownloadBackup_NotFoundWhenMissing(t *testing.T) {
	h, _ := newTicketBackupHandler(t, &ticketBackupFakeStore{})

	req := adminReq(httptest.NewRequest(http.MethodGet, "/api/admin/tickets/backups/tickets-missing.json/download", nil))
	req = mux.SetURLVars(req, map[string]string{"name": "tickets-missing.json"})
	rw := httptest.NewRecorder()
	h.DownloadBackup(rw, req)

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rw.Code, rw.Body.String())
	}
}

// ── DeleteBackup ──

func TestDeleteBackup_Success(t *testing.T) {
	h, backupsDir := newTicketBackupHandler(t, &ticketBackupFakeStore{})
	if err := os.MkdirAll(backupsDir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", backupsDir, err)
	}
	target := filepath.Join(backupsDir, "tickets-x.json")
	if err := os.WriteFile(target, []byte("{}"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := adminReq(httptest.NewRequest(http.MethodDelete, "/api/admin/tickets/backups/tickets-x.json", nil))
	req = mux.SetURLVars(req, map[string]string{"name": "tickets-x.json"})
	rw := httptest.NewRecorder()
	h.DeleteBackup(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("backup still present after delete, stat err = %v", err)
	}
}

// TestDeleteBackup_ProviderErrorSurfaces mirrors
// TestListBackups_ProviderErrorSurfaces for the delete path - see
// newTicketBackupS3FailFastHandler's doc comment for why an unreachable s3
// endpoint replaces the original fault-injected provider fake here too (the
// "path" backend equivalent is even less viable for delete specifically:
// os.RemoveAll, what LocalProvider.DeletePath calls, treats
// ERROR_PATH_NOT_FOUND as "already gone" and returns nil, so a file-blocked
// "directory" would not even surface as an error internally, let alone reach
// the handler).
func TestDeleteBackup_ProviderErrorSurfaces(t *testing.T) {
	disableAWSRetries(t)
	h := newTicketBackupS3FailFastHandler(t)

	req := adminReq(httptest.NewRequest(http.MethodDelete, "/api/admin/tickets/backups/tickets-x.json", nil))
	req = mux.SetURLVars(req, map[string]string{"name": "tickets-x.json"})
	rw := httptest.NewRecorder()
	h.DeleteBackup(rw, req)

	if rw.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", rw.Code, rw.Body.String())
	}
}

// ── InitRestore: provider-backed existence probe ──

// TestInitRestore_NotFoundWhenProviderMisses covers the os.Stat -> provider
// GetFile probe swap: a name that doesn't exist in the provider must 404
// before any token is issued.
func TestInitRestore_NotFoundWhenProviderMisses(t *testing.T) {
	fs := &ticketBackupFakeStore{user: &models.User{ID: "admin-1", Is2FAEnabled: true, TOTPSecret: "JBSWY3DPEHPK3PXP"}}
	h, _ := newTicketBackupHandler(t, fs)

	body, _ := json.Marshal(restoreInitRequest{Name: "tickets-missing.json"})
	req := adminReq(httptest.NewRequest(http.MethodPost, "/api/admin/tickets/restore/init", bytes.NewReader(body)))
	req = req.WithContext(context.WithValue(req.Context(), "userID", "admin-1"))
	rw := httptest.NewRecorder()
	h.InitRestore(rw, req)

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rw.Code, rw.Body.String())
	}
	if len(h.tokens) != 0 {
		t.Fatalf("a restore token was issued for a nonexistent backup: %d tokens", len(h.tokens))
	}
}

// adminReq stamps the admin flag every handler in ticket_migration.go now
// requires. The boundary itself is asserted in ticket_migration_admin_test.go;
// these tests are about what the handlers DO once a caller is past it.
func adminReq(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), "isAdmin", true))
}
