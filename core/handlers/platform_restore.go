package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"dylaris-core/services"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"
)

// Reading a platform bundle back.
//
// The database goes into a NEW database the operator names, never over the one
// Core is running on. Same shape as the in-panel database migration, and for
// the same reason: the live Core keeps working throughout, a failed restore
// costs nothing, and the operator switches over by restarting with the new DB_*
// values once they can see it worked.

// maxBundleMemory is what a multipart upload keeps in RAM before Go spools the
// rest to a temp file. A bundle is measured in gigabytes; this is only the
// threshold, not a limit on the upload.
const maxBundleMemory = 32 << 20

// bundleSourceTimeout bounds opening the operator's target database.
const bundleSourceTimeout = 10 * time.Second

type restoreTargetRequest struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DBName   string `json:"dbName"`
	SSLMode  string `json:"sslMode"`
}

func (t restoreTargetRequest) params() services.DBConnParams {
	return services.DBConnParams{
		Host:     strings.TrimSpace(t.Host),
		Port:     strings.TrimSpace(t.Port),
		User:     strings.TrimSpace(t.User),
		Password: t.Password,
		DBName:   strings.TrimSpace(t.DBName),
		SSLMode:  strings.TrimSpace(t.SSLMode),
	}
}

type restoreRequest struct {
	Passphrase string                            `json:"passphrase"`
	Components services.PlatformRestoreSelection `json:"components"`
	Target     restoreTargetRequest              `json:"target"`
	// Overwrite confirms a target that already holds tables. Refused without
	// it: a restore over an existing schema leaves rows from two installations
	// in one database with nothing afterwards saying which came from where.
	Overwrite bool `json:"overwrite"`
}

// InspectBundle POST /api/platform-backups/inspect
//
// Multipart: `bundle` is the file, `passphrase` is optional. Without it the
// plaintext header comes back and nothing else, which is enough to tell three
// downloaded bundles apart before typing anything.
func (h *PlatformBackupHandler) InspectBundle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxBundleMemory); err != nil {
		sendJSONError(w, "Could not read the upload", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("bundle")
	if err != nil {
		sendJSONError(w, "No bundle was uploaded", http.StatusBadRequest)
		return
	}
	defer file.Close()

	header, manifest, ierr := services.Inspect(file, r.FormValue("passphrase"))
	if header == nil {
		sendJSONError(w, "This is not a Dylaris backup bundle", http.StatusBadRequest)
		return
	}
	out := map[string]any{
		"success":   true,
		"source":    header.Source,
		"createdAt": header.CreatedAt,
		"schema":    header.Schema,
		// The passphrase is only CONFIRMED when the manifest came back: the
		// verifier in the header is what decides, and it decides before any
		// payload is decrypted.
		"passphraseOk": manifest != nil,
	}
	if manifest != nil {
		out["selection"] = manifest.Selection
		out["components"] = manifest.Components
		out["carriesClusterSecret"] = manifest.ClusterSecret != ""
	} else if ierr != nil && r.FormValue("passphrase") != "" {
		out["message"] = ierr.Error()
	}
	json.NewEncoder(w).Encode(out)
}

// RestoreBundle POST /api/platform-backups/restore
//
// Multipart: `bundle` is the file, `request` is the JSON above.
func (h *PlatformBackupHandler) RestoreBundle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxBundleMemory); err != nil {
		sendJSONError(w, "Could not read the upload", http.StatusBadRequest)
		return
	}
	var req restoreRequest
	if err := json.Unmarshal([]byte(r.FormValue("request")), &req); err != nil {
		sendJSONError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("bundle")
	if err != nil {
		sendJSONError(w, "No bundle was uploaded", http.StatusBadRequest)
		return
	}
	defer file.Close()

	h.runRestore(w, r, file, req)
}

// RestoreRun POST /api/platform-backups/runs/{id}/restore
//
// The same restore, reading the bundle out of the storage it was written to
// rather than back up through an upload. This is the ordinary case on the
// instance that made it; the upload is for carrying one to a different Dylaris.
func (h *PlatformBackupHandler) RestoreRun(w http.ResponseWriter, r *http.Request) {
	var req restoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid run id", http.StatusBadRequest)
		return
	}
	run, err := h.state.Store.GetPlatformBackupRun(id)
	if err != nil || run == nil {
		sendJSONError(w, "Run not found", http.StatusNotFound)
		return
	}
	if run.Status != "success" || run.StorageKey == "" {
		sendJSONError(w, "This run produced no archive", http.StatusConflict)
		return
	}
	bs, err := services.ResolveJobStorage(h.state.Store, run.StorageID, "")
	if err != nil {
		sendJSONError(w, "The storage this run wrote to is not available", http.StatusConflict)
		return
	}
	st, err := backupstorage.Open(r.Context(), bs, NewBackupHandler(h.state).backupDeps())
	if err != nil {
		sendJSONError(w, "Could not open the storage", 500)
		return
	}
	rc, err := st.Get(r.Context(), run.StorageKey)
	if err != nil {
		sendJSONError(w, "The archive could not be read", 500)
		return
	}
	defer rc.Close()

	h.runRestore(w, r, rc, req)
}

func (h *PlatformBackupHandler) runRestore(w http.ResponseWriter, r *http.Request, src io.Reader, req restoreRequest) {
	if req.Passphrase == "" {
		sendJSONError(w, "The backup passphrase is required", http.StatusBadRequest)
		return
	}
	if !req.Components.Any() {
		sendJSONError(w, services.ErrNothingSelected.Error(), http.StatusBadRequest)
		return
	}

	restorer, cleanup, err := h.restorer(r.Context(), req)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if cleanup != nil {
		defer cleanup()
	}

	res, rerr := restorer.Restore(r.Context(), src, req.Passphrase, req.Components, req.Overwrite)
	if rerr != nil {
		status := 500
		switch {
		case errors.Is(rerr, services.ErrTargetNotEmpty), errors.Is(rerr, services.ErrNothingSelected):
			status = http.StatusConflict
		case errors.Is(rerr, services.ErrBundleWrongPassphrase):
			status = http.StatusForbidden
		}
		w.WriteHeader(status)
		// The partial result is returned with the failure: an operator needs to
		// know which components landed before it stopped, not only that it did.
		json.NewEncoder(w).Encode(map[string]any{"success": false, "message": rerr.Error(), "result": res})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "result": res})
}

// restorer builds the restorer for one request, and returns the cleanup for the
// connection it may have opened to the operator's target database.
func (h *PlatformBackupHandler) restorer(ctx context.Context, req restoreRequest) (*services.PlatformRestorer, func(), error) {
	rst := &services.PlatformRestorer{
		ClusterSecret: h.state.ClusterSecret,
		WorkDir:       h.state.PlatformBackupWorkDir,
		OpenCoreStorageWriter: func(area string) (services.CoreStorageWriter, error) {
			prefix := CoreStoragePrefixLibrary
			if area == "modpacks" {
				prefix = CoreStoragePrefixModpacks
			}
			prov, err := h.state.buildCoreStorageProvider(prefix)
			if err != nil {
				return nil, err
			}
			return &coreStorageAreaWriter{prov: prov}, nil
		},
	}
	if !req.Components.Database {
		// No target is needed, and asking for one would refuse a perfectly good
		// "restore only the Library".
		return rst, nil, nil
	}

	params := req.Target.params()
	if params.Host == "" || params.DBName == "" || params.User == "" {
		return nil, nil, errors.New("restoring the database needs a target database to restore into")
	}
	db, err := params.Open(ctx, bundleSourceTimeout)
	if err != nil {
		return nil, nil, err
	}
	major, err := services.PGServerMajor(db)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	pg := params.PG()

	rst.RestoreDatabaseInto = func(ctx context.Context, src io.Reader) error {
		_, rerr := services.RestoreDatabase(ctx, pg, major, src)
		return rerr
	}
	rst.TargetIsEmpty = func(ctx context.Context) (bool, error) { return targetIsEmpty(ctx, db) }
	rst.OpenTarget = func(context.Context) (services.ResealTarget, io.Closer, error) {
		// The SAME connection the emptiness check used, deliberately: opening a
		// second one to a database that was just rewritten is one more way for
		// the reseal to run against something other than what was restored.
		return store.NewPostgresStore(db), nil, nil
	}
	return rst, func() { db.Close() }, nil
}

// targetIsEmpty reports whether the operator's database holds any table of its
// own yet.
func targetIsEmpty(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// coreStorageAreaWriter writes one object into a scoped Core storage area.
type coreStorageAreaWriter struct {
	prov interface {
		WriteFile(ctx context.Context, path string, content io.Reader) error
	}
}

func (w *coreStorageAreaWriter) Write(ctx context.Context, key string, r io.Reader) error {
	return w.prov.WriteFile(ctx, key, r)
}
