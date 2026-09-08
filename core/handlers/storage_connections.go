package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// StorageConnectionsHandler serves CRUD + a test probe for the shared, named
// storage connections (currently s3). Note the plural: the singular
// StorageConnectionHandler in storage_connection.go is a different thing (the
// core-storage backend health snapshot), so the two must not be conflated.
type StorageConnectionsHandler struct {
	state *AppState
}

func NewStorageConnectionsHandler(state *AppState) *StorageConnectionsHandler {
	return &StorageConnectionsHandler{state: state}
}

// storageConnectionRequest is the write DTO. The secret arrives here (a plain
// json field) but the model's SecretAccessKey is json:"-", so the model can
// neither receive it from nor leak it to the wire; the handler bridges the two.
type storageConnectionRequest struct {
	Name            string          `json:"name"`
	Provider        string          `json:"provider"`
	Config          json.RawMessage `json:"config"`
	AccessKey       string          `json:"accessKey"`
	SecretAccessKey string          `json:"secretAccessKey"`
}

// storageConnectionConfig is the non-secret portion of a connection's config
// JSONB. The access key and the secret live in their own columns, never here.
type storageConnectionConfig struct {
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	ForcePathStyle bool   `json:"forcePathStyle"`
	Prefix         string `json:"prefix"`
}

// validStorageConnectionProvider allowlists the providers a connection may use.
// Only s3 is credentialed today; the table has room for others later.
// validateStorageConnectionEndpoint runs the same fail-closed S3-endpoint check
// core-storage and modpacks use (reject a credential-bearing '@', require a
// parseable URL). Storage-connections previously skipped it, so a credentialed
// endpoint could be stored here though rejected everywhere else. Empty config is
// allowed (a metadata-only update).
func validateStorageConnectionEndpoint(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var cfg storageConnectionConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("invalid config JSON")
	}
	return validateS3Endpoint("storage connection", cfg.Endpoint)
}

func validStorageConnectionProvider(p string) bool {
	return p == "s3"
}

// storageConnectionIdentity extracts the three fields that decide WHERE a
// stored s3 secret gets used. Same trio mergeCoreStorageCandidate and
// mergeBackupStorageSecret compare; the endpoint and bucket live in the config
// JSONB here, the access key in its own column.
func storageConnectionIdentity(raw json.RawMessage, accessKey string) (endpoint, bucket, key string) {
	var cfg storageConnectionConfig
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	return cfg.Endpoint, cfg.Bucket, accessKey
}

// errStorageConnectionSecretRequired is returned when an edit moves where the
// stored secret would be used without supplying a new one.
var errStorageConnectionSecretRequired = errors.New(
	"the endpoint, bucket or access key changed, so the stored secret cannot be reused - re-enter the secret access key with this change")

// storageConnectionSecretRebound reports whether this update would point the
// STORED secret somewhere else without the caller having supplied a new one.
//
// The same guard core storage has always had (mergeCoreStorageCandidate) and
// backup storages gained later (mergeBackupStorageSecret), for the same
// reason, which applies harder here because a storage connection is the shared
// credential core storage and modpack storage both reference by id:
//
//   - Security. settings.write is a delegatable panel capability, and no read
//     path ever returns the secret (SecretAccessKey is json:"-", the list path
//     does not even decrypt). A holder who cannot READ the secret could point
//     it at an endpoint and bucket of their choosing simply by submitting
//     those with the secret field left blank. SigV4 signs with the secret
//     rather than sending it, so this is not a plaintext leak - it is
//     credential rebinding: an attacker-chosen host receives validly signed
//     requests carrying the operator's data.
//   - Usability. Changing only the access key while leaving the secret blank
//     (because the form never shows it) would otherwise persist a NEW access
//     key paired with the OLD secret, and every later read fails a signature
//     check with nothing pointing at the edit that caused it.
//
// A submitted secret is a genuine rotation and is always allowed. A connection
// with no stored secret has nothing to rebind.
func storageConnectionSecretRebound(req storageConnectionRequest, existing *models.StorageConnection) bool {
	if req.SecretAccessKey != "" || existing == nil || !existing.SecretSet {
		return false
	}
	inEndpoint, inBucket, inKey := storageConnectionIdentity(req.Config, req.AccessKey)
	exEndpoint, exBucket, exKey := storageConnectionIdentity(existing.Config, existing.AccessKey)
	return inEndpoint != exEndpoint || inBucket != exBucket || inKey != exKey
}

// ListConnections GET /api/storage-connections. The secret never appears in the
// response: SecretAccessKey is json:"-" and the store does not decrypt on the
// list path, so only the SecretSet flag reports that one is stored.
func (h *StorageConnectionsHandler) ListConnections(w http.ResponseWriter, r *http.Request) {
	conns, err := h.state.Store.ListStorageConnections()
	if err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	if conns == nil {
		conns = []models.StorageConnection{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "connections": conns})
}

// CreateConnection POST /api/storage-connections - adds a reusable storage
// connection. Only the s3 provider exists today, and a duplicate name is 409.
func (h *StorageConnectionsHandler) CreateConnection(w http.ResponseWriter, r *http.Request) {
	var req storageConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	if req.Name == "" || req.Provider == "" {
		sendJSONError(w, "name and provider are required", 400)
		return
	}
	if !validStorageConnectionProvider(req.Provider) {
		sendJSONError(w, "invalid provider (expected s3)", 400)
		return
	}
	if err := validateStorageConnectionEndpoint(req.Config); err != nil {
		sendJSONError(w, err.Error(), 400)
		return
	}
	conn := models.StorageConnection{
		Name:            req.Name,
		Provider:        req.Provider,
		Config:          req.Config,
		AccessKey:       req.AccessKey,
		SecretAccessKey: req.SecretAccessKey,
	}
	id, err := h.state.Store.CreateStorageConnection(&conn)
	if err != nil {
		if errors.Is(err, store.ErrNameTaken) {
			sendJSONError(w, "A storage connection with that name already exists", 409)
			return
		}
		sendJSONError(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": id})
}

// UpdateConnection PATCH /api/storage-connections/{id}. The secret is write-only:
// the metadata update never touches secret_enc, and the secret is rotated only
// when a non-blank value was submitted. A blank secret (the "leave blank to
// keep" case) therefore preserves the stored credential.
func (h *StorageConnectionsHandler) UpdateConnection(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid ID", 400)
		return
	}
	var req storageConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	if !validStorageConnectionProvider(req.Provider) {
		sendJSONError(w, "invalid provider (expected s3)", 400)
		return
	}
	if err := validateStorageConnectionEndpoint(req.Config); err != nil {
		sendJSONError(w, err.Error(), 400)
		return
	}
	existing, gerr := h.state.Store.GetStorageConnection(id)
	if gerr != nil {
		if errors.Is(gerr, sql.ErrNoRows) {
			sendJSONError(w, "Storage connection not found", 404)
			return
		}
		sendJSONError(w, "Database error", 500)
		return
	}
	if storageConnectionSecretRebound(req, existing) {
		sendJSONError(w, errStorageConnectionSecretRequired.Error(), 400)
		return
	}
	conn := models.StorageConnection{
		ID:        id,
		Name:      req.Name,
		Provider:  req.Provider,
		Config:    req.Config,
		AccessKey: req.AccessKey,
	}
	if err := h.state.Store.UpdateStorageConnection(&conn); err != nil {
		switch {
		case errors.Is(err, store.ErrNameTaken):
			sendJSONError(w, "A storage connection with that name already exists", 409)
		case errors.Is(err, sql.ErrNoRows):
			sendJSONError(w, "Storage connection not found", 404)
		default:
			sendJSONError(w, err.Error(), 500)
		}
		return
	}
	if req.SecretAccessKey != "" {
		if err := h.state.Store.SetStorageConnectionSecret(id, req.SecretAccessKey); err != nil {
			sendJSONError(w, err.Error(), 500)
			return
		}
	}
	// A modpack setting can point at this connection, and the cached "is modpack
	// storage configured" answer is what the panel gates the create button on.
	h.state.InvalidateModpackStorage()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// DeleteConnection DELETE /api/storage-connections/{id} - removes a storage
// connection.
func (h *StorageConnectionsHandler) DeleteConnection(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid ID", 400)
		return
	}
	if err := h.state.Store.DeleteStorageConnection(id); err != nil {
		sendJSONError(w, err.Error(), 500)
		return
	}
	// A modpack setting can point at this connection, and the cached "is modpack
	// storage configured" answer is what the panel gates the create button on.
	h.state.InvalidateModpackStorage()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// TestConnection POST /api/storage-connections/{id}/test. Loads the saved
// connection (which decrypts the secret), builds a provider and runs the
// write/read-back/delete probe. Like the core-storage and backup probes it
// returns HTTP 200 with an "ok" verdict even on a failed probe - the transport
// succeeded; the verdict is the payload.
func (h *StorageConnectionsHandler) TestConnection(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid ID", 400)
		return
	}
	conn, err := h.state.Store.GetStorageConnection(id)
	if err != nil {
		sendJSONError(w, "Connection not found", 404)
		return
	}
	prov, err := storageConnectionProvider(conn)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "ok": false, "stage": services.StageRejected, "message": err.Error(),
		})
		return
	}
	writeStorageProbeVerdict(w, r, conn, prov)
}

// writeStorageProbeVerdict answers a storage test in two steps: does the
// endpoint answer, and only then, does it accept these credentials for this
// bucket.
//
// Shared by the saved and the draft test because they ask the identical
// question of the identical provider - the only difference is where the
// connection came from, which the reader of the result does not care about.
func writeStorageProbeVerdict(w http.ResponseWriter, r *http.Request, conn *models.StorageConnection, prov storage.StorageProvider) {
	endpoint, _, _ := storageConnectionIdentity(conn.Config, conn.AccessKey)
	if host, port, ok := services.HostPortFromEndpoint(endpoint); ok {
		if reach := services.Reachable(r.Context(), host, port); !reach.OK {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "ok": false, "stage": reach.Stage, "message": reach.Message,
			})
			return
		}
	}
	ok, message := probeStorageProvider(r.Context(), prov)
	if !ok {
		check := services.ClassifyStorageFailure(message)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "ok": false, "stage": check.Stage, "message": check.Message,
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "ok": true, "stage": services.StageOK, "message": message,
	})
}

// errStorageConnectionTestNeedsSecret is returned when an unsaved test has no
// credential to run with and no saved one it is allowed to borrow.
var errStorageConnectionTestNeedsSecret = errors.New(
	"enter the secret access key to test this connection")

// TestDraftConnection POST /api/storage-connections/test. Runs the same probe
// against a connection that has NOT been saved.
//
// It exists because the only way to find out whether an endpoint, a bucket and a
// pair of keys actually work was to save them first, and a saved-but-wrong
// connection is a thing other screens can already select. Testing before
// committing is the order everyone tries anyway.
//
// The interesting part is which credential it is allowed to use. `settings.write`
// is delegatable and no read path ever returns a stored secret, so a bodied test
// that happily borrowed the saved secret for an operator-supplied endpoint would
// be a credential-rebinding oracle: point it at a host you control and receive
// validly signed requests. So the stored secret is borrowed only when the
// endpoint, bucket and access key are all unchanged - the same trio and the same
// rule storageConnectionSecretRebound applies to a save.
func (h *StorageConnectionsHandler) TestDraftConnection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		storageConnectionRequest
		// ID names the saved connection whose secret may be borrowed. Zero for
		// a brand new one, which then has to carry its own.
		ID int `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if !validStorageConnectionProvider(req.Provider) {
		sendJSONError(w, "Unsupported provider", http.StatusBadRequest)
		return
	}
	// The submitted endpoint reaches a dialer, so it gets the same fail-closed
	// check a save gets, at the point where the operator can still read the
	// error.
	if err := validateStorageConnectionEndpoint(req.Config); err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	conn := &models.StorageConnection{
		Provider:        req.Provider,
		Config:          req.Config,
		AccessKey:       req.AccessKey,
		SecretAccessKey: req.SecretAccessKey,
	}

	if conn.SecretAccessKey == "" {
		if req.ID == 0 {
			sendJSONError(w, errStorageConnectionTestNeedsSecret.Error(), http.StatusBadRequest)
			return
		}
		existing, err := h.state.Store.GetStorageConnection(req.ID)
		if err != nil {
			sendJSONError(w, "Connection not found", http.StatusNotFound)
			return
		}
		if storageConnectionSecretRebound(req.storageConnectionRequest, existing) {
			sendJSONError(w, errStorageConnectionSecretRequired.Error(), http.StatusBadRequest)
			return
		}
		if !existing.SecretSet {
			sendJSONError(w, errStorageConnectionTestNeedsSecret.Error(), http.StatusBadRequest)
			return
		}
		conn.SecretAccessKey = existing.SecretAccessKey
	}

	prov, err := storageConnectionProvider(conn)
	if err != nil {
		// 200 with a verdict, like the saved-connection probe: the request
		// worked, the configuration did not.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "ok": false, "stage": services.StageRejected, "message": err.Error(),
		})
		return
	}
	writeStorageProbeVerdict(w, r, conn, prov)
}

// storageConnectionProvider builds a storage.StorageProvider from a resolved
// connection (its secret already decrypted into SecretAccessKey). The access key
// and secret come from their own columns; the rest from the config JSONB.
func storageConnectionProvider(conn *models.StorageConnection) (storage.StorageProvider, error) {
	if conn.Provider != "s3" {
		return nil, fmt.Errorf("unsupported provider %q", conn.Provider)
	}
	var cfg storageConnectionConfig
	if len(conn.Config) > 0 {
		if err := json.Unmarshal(conn.Config, &cfg); err != nil {
			return nil, fmt.Errorf("invalid config: %w", err)
		}
	}
	return storage.NewProvider("s3", "", map[string]string{
		storage.OptS3Endpoint:  cfg.Endpoint,
		storage.OptS3Region:    cfg.Region,
		storage.OptS3Bucket:    cfg.Bucket,
		storage.OptS3AccessKey: conn.AccessKey,
		storage.OptS3SecretKey: conn.SecretAccessKey,
		storage.OptS3PathStyle: boolStr(cfg.ForcePathStyle),
		storage.OptS3Prefix:    cfg.Prefix,
	})
}
