package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/services"
	backupstorage "dylaris-core/storage/backup"
)

// Maintenance of the platform's OWN object storage: the orphan scan and the
// bucket lifecycle rules. Platform storages only (owner_id NULL) - a tenant's
// bucket answers 404 here exactly as on every other admin storage route.

// errNotObjectStorage refuses storages that are not an S3-compatible bucket:
// node-local archives live on customer machines, and a shared path has no
// lifecycle rules to set.
var errNotObjectStorage = errors.New("only object storage (S3/R2) can be scanned or given lifecycle rules")

// platformObjectStore opens a platform storage as the raw bucket client, scoped
// to exactly the folder its backups are written under, so List returns the same
// storage-relative keys the run rows hold - and, unlike the core-storage
// adapter, real modification times.
//
// The prefix rules mirror backupstorage.Open and newStorageProviderForConfig;
// a mismatch would scan a different folder from the one backups go to.
func (h *BackupHandler) platformObjectStore(ctx context.Context, bs *models.BackupStorage) (*backupstorage.S3Storage, error) {
	var cfg CoreStorageConfig
	var sub string
	switch bs.Provider {
	case "s3":
		return backupstorage.NewS3(ctx, bs.Config)
	case "core-storage":
		c, err := h.state.effectiveCoreStorageConfig()
		if err != nil {
			return nil, err
		}
		cfg, sub = c, backupstorage.CoreStorageSubPrefix
	case "connection":
		var cc backupstorage.ConnectionConfig
		if len(bs.Config) > 0 {
			if err := json.Unmarshal(bs.Config, &cc); err != nil {
				return nil, fmt.Errorf("invalid connection config: %w", err)
			}
		}
		conn, err := h.state.Store.GetStorageConnection(cc.ConnectionID)
		if err != nil || conn == nil {
			return nil, fmt.Errorf("storage connection %d could not be loaded", cc.ConnectionID)
		}
		cfg, sub = coreStorageConfigFromConnection(conn), cc.Prefix
		if strings.TrimSpace(sub) == "" {
			sub = backupstorage.CoreStorageSubPrefix
		}
	default:
		return nil, errNotObjectStorage
	}
	if cfg.Backend != "s3" {
		return nil, errNotObjectStorage
	}
	prefix := sub
	if cfg.S3Prefix != "" {
		prefix = cfg.S3Prefix + "/" + sub
	}
	raw, err := json.Marshal(backupstorage.S3Config{
		Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
		AccessKeyID: cfg.S3AccessKey, SecretAccessKey: cfg.S3SecretKey,
		ForcePathStyle: cfg.S3PathStyle, Prefix: prefix,
	})
	if err != nil {
		return nil, err
	}
	return backupstorage.NewS3(ctx, raw)
}

// orphanLocks serializes scans and deletes per storage: a scan lists the whole
// bucket, and two of them at once buy nothing.
//
// ponytail: per process. Two Core replicas can each run one; move to a Redis
// lock if that ever matters.
var orphanLocks sync.Map // storage id -> *sync.Mutex

func lockStorageMaintenance(id int) (unlock func(), ok bool) {
	m, _ := orphanLocks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// openMaintenanceTarget is the shared prologue: the id, a platform storage, and
// its bucket.
func (h *BackupHandler) openMaintenanceTarget(w http.ResponseWriter, r *http.Request) (*models.BackupStorage, *backupstorage.S3Storage, bool) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid ID", http.StatusBadRequest)
		return nil, nil, false
	}
	bs, ok := h.platformStorage(w, id)
	if !ok {
		return nil, nil, false
	}
	st, err := h.platformObjectStore(r.Context(), bs)
	if err != nil {
		if errors.Is(err, errNotObjectStorage) {
			sendJSONError(w, err.Error(), http.StatusConflict)
			return nil, nil, false
		}
		sendJSONError(w, "Could not open the storage: "+err.Error(), http.StatusBadGateway)
		return nil, nil, false
	}
	return bs, st, true
}

// orphanLookups answers whether a server or job still exists. A store error
// answers "exists": the answer decides whether a file may be deleted without
// the extra confirmation, so not knowing must never read as "gone".
func (h *BackupHandler) orphanLookups() services.OrphanLookups {
	exists := func(found bool, err error) bool {
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		return err != nil || found
	}
	return services.OrphanLookups{
		ServerExists: func(uuid string) bool {
			srv, err := h.state.Store.GetServerByUUID(uuid)
			return exists(srv != nil, err)
		},
		BackupJobExists: func(id int) bool {
			j, err := h.state.Store.GetBackupJob(id)
			return exists(j != nil, err)
		},
		PlatformBackupExists: func(id int) bool {
			j, err := h.state.Store.GetPlatformBackupJob(id)
			return exists(j != nil, err)
		},
	}
}

func (h *BackupHandler) orphanRefs() services.OrphanRefs {
	return services.StoreOrphanRefs(h.state.Store, h.state.Redis)
}

// ScanOrphans GET /api/admin/storage/{id}/orphans - dry run. Lists the platform
// storage and reports the objects Core wrote that no backup row names any more
// and that are older than 24 hours, with whether their server and job still
// exist. Files Core does not recognise are only counted. Deletes nothing.
// 409 while a scan or delete of the same storage is running, or for a storage
// that is not object storage.
func (h *BackupHandler) ScanOrphans(w http.ResponseWriter, r *http.Request) {
	bs, st, ok := h.openMaintenanceTarget(w, r)
	if !ok {
		return
	}
	unlock, ok := lockStorageMaintenance(bs.ID)
	if !ok {
		sendJSONError(w, "A scan or delete of this storage is already running", http.StatusConflict)
		return
	}
	defer unlock()

	scan, err := services.ScanOrphans(r.Context(), st, h.orphanRefs(), h.orphanLookups(), time.Now())
	if err != nil {
		sendJSONError(w, "Scan failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "scan": scan})
}

type deleteOrphansRequest struct {
	Keys []string `json:"keys"`
	// IncludeLive allows deleting files whose server and job still exist (see
	// services.OrphanScan.Live). Per request on purpose: it is the operator
	// saying "I know these are not lost backups", never a setting.
	IncludeLive bool `json:"includeLive"`
}

// DeleteOrphans POST /api/admin/storage/{id}/orphans/delete - deletes the
// listed keys (at most 1000) from a platform storage. Each key is checked
// again right before its delete: it must still be a file Core writes, named by
// no backup row and older than 24 hours, or it is left alone. A file whose
// server and backup job still exist is refused unless includeLive is true.
// Answers one result per key.
func (h *BackupHandler) DeleteOrphans(w http.ResponseWriter, r *http.Request) {
	var req deleteOrphansRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Keys) == 0 {
		sendJSONError(w, "No keys given", http.StatusBadRequest)
		return
	}
	if len(req.Keys) > services.MaxOrphanDeleteKeys {
		sendJSONError(w, fmt.Sprintf("At most %d keys per request", services.MaxOrphanDeleteKeys), http.StatusBadRequest)
		return
	}
	bs, st, ok := h.openMaintenanceTarget(w, r)
	if !ok {
		return
	}
	unlock, ok := lockStorageMaintenance(bs.ID)
	if !ok {
		sendJSONError(w, "A scan or delete of this storage is already running", http.StatusConflict)
		return
	}
	defer unlock()

	results, err := services.DeleteOrphans(r.Context(), st, h.orphanRefs(), h.orphanLookups(), req.Keys, req.IncludeLive, time.Now)
	var deleted int
	var bytes int64
	var deletedKeys []string
	for _, res := range results {
		if res.Deleted {
			deleted++
			bytes += res.Size
			deletedKeys = append(deletedKeys, res.Key)
		}
	}
	// Audited whatever happened: a partial run deleted real files.
	actorID, _ := r.Context().Value("userID").(string)
	LogIdentityAudit(h.state, r, AuditEventStorageOrphansDeleted, actorID, "", map[string]interface{}{
		"storageId":   bs.ID,
		"requested":   len(req.Keys),
		"includeLive": req.IncludeLive,
		"deleted":     deleted,
		"bytes":       bytes,
		"keys":        deletedKeys,
	})
	resp := map[string]any{"success": err == nil, "results": results, "deleted": deleted, "deletedBytes": bytes}
	if err != nil {
		resp["message"] = "Stopped early: " + err.Error()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
	}
	json.NewEncoder(w).Encode(resp)
}

// lifecycleRuleView is one bucket lifecycle rule as the panel shows it.
type lifecycleRuleView struct {
	ID                 string `json:"id"`
	Status             string `json:"status"`
	Prefix             string `json:"prefix"`
	ExpirationDays     int32  `json:"expirationDays,omitempty"`
	AbortMultipartDays int32  `json:"abortMultipartDays,omitempty"`
	Ours               bool   `json:"ours"`
}

func viewLifecycleRules(rules []types.LifecycleRule) []lifecycleRuleView {
	out := make([]lifecycleRuleView, 0, len(rules))
	for _, r := range rules {
		v := lifecycleRuleView{ID: aws.ToString(r.ID), Status: string(r.Status), Ours: backupstorage.IsDylarisLifecycleRule(r)}
		// The rule-level Prefix is deprecated, but older buckets still answer with it.
		v.Prefix = aws.ToString(r.Prefix)
		if r.Filter != nil {
			if r.Filter.Prefix != nil {
				v.Prefix = aws.ToString(r.Filter.Prefix)
			} else if r.Filter.And != nil {
				v.Prefix = aws.ToString(r.Filter.And.Prefix)
			}
		}
		if r.Expiration != nil {
			v.ExpirationDays = aws.ToInt32(r.Expiration.Days)
		}
		if r.AbortIncompleteMultipartUpload != nil {
			v.AbortMultipartDays = aws.ToInt32(r.AbortIncompleteMultipartUpload.DaysAfterInitiation)
		}
		out = append(out, v)
	}
	return out
}

// plannedLifecycle is what Core sets on this storage's bucket. Migration
// transfers are only ever written to a storage of provider s3 (the platform
// default, see MigrationOrchestrator.transferViaR2), so only that kind gets the
// expiry rule, and it outlives the transfer's presigned URLs.
func (h *BackupHandler) plannedLifecycle(bs *models.BackupStorage, st *backupstorage.S3Storage) []types.LifecycleRule {
	var days int32
	if bs.Provider == "s3" {
		days = backupstorage.MigrationExpiryDays(services.MigrationTransferTTL(h.state.Store))
	}
	return backupstorage.DylarisLifecycleRules(st.KeyPrefix(), days)
}

func sendLifecycleError(w http.ResponseWriter, err error) {
	if errors.Is(err, backupstorage.ErrLifecycleForbidden) {
		sendJSONErrorCode(w,
			"The credentials of this storage may not read or change the bucket's lifecycle rules. "+
				"Use a key with bucket administration rights (on R2: an Admin Read & Write token), "+
				"or set the rules in the provider's dashboard. Backups are not affected.",
			"lifecycle_forbidden", http.StatusConflict)
		return
	}
	sendJSONError(w, err.Error(), http.StatusBadGateway)
}

// GetLifecycle GET /api/admin/storage/{id}/lifecycle - the bucket's current
// lifecycle rules, with Core's own marked, and the rules Apply would set.
func (h *BackupHandler) GetLifecycle(w http.ResponseWriter, r *http.Request) {
	bs, st, ok := h.openMaintenanceTarget(w, r)
	if !ok {
		return
	}
	rules, err := st.LifecycleRules(r.Context())
	if err != nil {
		sendLifecycleError(w, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"bucket":  st.Bucket(),
		"rules":   viewLifecycleRules(rules),
		"planned": viewLifecycleRules(h.plannedLifecycle(bs, st)),
	})
}

// ApplyLifecycle POST /api/admin/storage/{id}/lifecycle - merges Core's rules
// into the bucket's lifecycle configuration: incomplete multipart uploads under
// the storage's folder are aborted after 3 days, and on the storage that
// receives migration transfers those expire one day after their download links
// (at least 2 days). Rules whose id does not start with "dylaris-" are kept.
func (h *BackupHandler) ApplyLifecycle(w http.ResponseWriter, r *http.Request) {
	bs, st, ok := h.openMaintenanceTarget(w, r)
	if !ok {
		return
	}
	// Read-merge-write of one document: two applies at once would each write
	// back only what they read.
	unlock, ok := lockStorageMaintenance(bs.ID)
	if !ok {
		sendJSONError(w, "Another maintenance action on this storage is running", http.StatusConflict)
		return
	}
	defer unlock()
	existing, err := st.LifecycleRules(r.Context())
	if err != nil {
		sendLifecycleError(w, err)
		return
	}
	merged := backupstorage.MergeLifecycleRules(existing, h.plannedLifecycle(bs, st))
	if err := st.PutLifecycleRules(r.Context(), merged); err != nil {
		sendLifecycleError(w, err)
		return
	}
	actorID, _ := r.Context().Value("userID").(string)
	LogIdentityAudit(h.state, r, AuditEventStorageLifecycleSet, actorID, "", map[string]interface{}{
		"storageId": bs.ID,
		"bucket":    st.Bucket(),
		"rules":     len(merged),
	})
	json.NewEncoder(w).Encode(map[string]any{"success": true, "bucket": st.Bucket(), "rules": viewLifecycleRules(merged)})
}
