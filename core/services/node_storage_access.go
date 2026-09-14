package services

import (
	"context"
	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"dylaris-pkg/release"

	"github.com/redis/go-redis/v9"
)

// Presigned-URL TTLs for the cross-LAN migration transfer (migration_orchestrator.go).
// Backups and restores no longer use them: their URLs are minted when the node
// starts the transfer, not at dispatch. Two knobs because a BYON tenant's
// home uplink can be much slower than an operator DC node, so its presigned URL
// must stay valid long enough to finish a multi-GB transfer.
const (
	PresignTTLNodeKey        = "r2.presign_ttl_node_minutes" // operator nodes
	PresignTTLBYONKey        = "r2.presign_ttl_byon_minutes" // BYON tenant nodes
	DefaultPresignTTLNodeMin = 60                            // 1h
	DefaultPresignTTLBYONMin = 360                           // 6h
)

func presignTTL(st store.Store, isBYON bool) time.Duration {
	key, def := PresignTTLNodeKey, DefaultPresignTTLNodeMin
	if isBYON {
		key, def = PresignTTLBYONKey, DefaultPresignTTLBYONMin
	}
	mins := def
	if v, _ := st.GetSetting(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mins = n
		}
	}
	return time.Duration(mins) * time.Minute
}

// nodeResolvesProvider reports whether the NODE handles a backup provider by
// itself. The list mirrors the filesystem cases of the switch in
// platform/node/backup_worker.go (uploadBackup) and backup_restore.go
// (downloadBackup).
//
// Everything else is object storage reached through Core. "s3" used to be on
// this list, which is how an operator node came to receive a bucket's
// credentials in its command; "connection" and "core-storage" are indirections
// the node could never resolve at all. For all of them Core now drives the
// transfer and the node only ever sees presigned URLs, asked for when the
// transfer starts (see backup_transfer.go).
func nodeResolvesProvider(provider string) bool {
	switch provider {
	case "local", "shared", "node-local":
		return true
	}
	return false
}

// presignedMultipartSince is the release in which the node learned to upload a
// backup as presigned multipart parts and to ask Core for a restore URL. An
// older node would read a command without a URL or credentials as an unknown
// or unusable provider - and a restore stops the server before it finds out.
//
// A constant rather than the running Core's own RELEASE_VERSION, for the reason
// modReportingSince gives: a development build carries no version.
const presignedMultipartSince = "2026.09.14.2"

// ErrNodeUpdateRequired refuses a backup or restore on object storage for a node
// older than presignedMultipartSince, or one whose version is unknown. There is
// no fallback on purpose: the only one would be handing the node credentials.
var ErrNodeUpdateRequired = errors.New("this node must be updated before it can back up to or restore from object storage")

// nodeTakesPresignedTransfers reports whether the node's heartbeat names a
// release that can take a presigned-only backup or restore. No heartbeat, an
// unparseable version or an older one counts as old, the safe direction:
// refusing costs one clear failure, guessing wrong costs a restore that stops a
// server and cannot continue.
//
// A heartbeat that names no version at all is a development build (the testbed,
// a local build), which is built from the source it runs against and is let
// through. Every image CI builds is stamped, so no production node reports "".
func nodeTakesPresignedTransfers(ctx context.Context, rdb *redis.Client, nodeToken string) bool {
	hb := LoadHeartbeat(ctx, rdb, nodeToken)
	if hb == nil {
		return false
	}
	if hb.ReleaseVersion == "" {
		return true
	}
	have, err := release.ParseVersion(hb.ReleaseVersion)
	if err != nil {
		return false
	}
	since, err := release.ParseVersion(presignedMultipartSince)
	if err != nil {
		return false
	}
	return have.Compare(since) >= 0
}

// PrepareNodeStorage decides what a node receives for a backup upload or a
// restore download, and whether the transfer goes through Core-presigned URLs.
//
//   - A filesystem provider the node handles itself (local, shared, node-local):
//     the full storage blob, which holds a path and no secret. objectStorage is
//     false.
//   - Anything else is object storage: a CREDENTIAL-STRIPPED blob and
//     objectStorage true. The command then carries no URL and no credentials;
//     the node asks Core for part URLs or a restore URL when it starts, for
//     every node, owned or not.
//
// For object storage it also refuses, with an error the caller fails the run
// with, when Core cannot open the target (the row names a connection that is
// gone, a builder is missing) or when the node is too old to take a
// presigned-only transfer (ErrNodeUpdateRequired). Both are decided BEFORE
// anything is dispatched: a restore stops the server first, so a refusal that
// only surfaced on the node would take the server down for nothing.
func PrepareNodeStorage(ctx context.Context, rdb *redis.Client, storage *models.BackupStorage, node *models.Node, deps backupstorage.Deps) (storageJSON []byte, objectStorage bool, err error) {
	full, _ := json.Marshal(storage)
	if storage == nil || nodeResolvesProvider(storage.Provider) {
		return full, false, nil
	}

	stripped := *storage
	stripped.Config = json.RawMessage(`{}`)
	strippedJSON, _ := json.Marshal(&stripped)

	if _, err := backupstorage.Open(ctx, storage, deps); err != nil {
		return strippedJSON, true, fmt.Errorf("backup target %q (%s) cannot be reached by a node: %w", storage.Name, storage.Provider, err)
	}
	token := ""
	if node != nil {
		token = node.Token
	}
	if !nodeTakesPresignedTransfers(ctx, rdb, token) {
		return strippedJSON, true, ErrNodeUpdateRequired
	}
	return strippedJSON, true, nil
}
