package handlers

import (
	"encoding/json"
	"net/http"

	"dylaris-core/services/storagereach"
)

// FeatureModpacks is the canonical name used in X-Feature-Disabled headers
// + JSON error responses for the modpacks feature.
const FeatureModpacks = "modpacks"

// FeatureTickets is the canonical name used in X-Feature-Disabled headers
// + JSON error responses for the ticket subsystem.
const FeatureTickets = "tickets"

// FeatureShareLinks is the canonical name for the modpack share-links sub-feature.
const FeatureShareLinks = "modpack_share_links"

// FeatureModpackAuthoring is the canonical name for the end-user half of the
// modpack switch: the subsystem is on, but authoring is not open to users.
const FeatureModpackAuthoring = "modpack_authoring"

// FeatureCoreStorage is the canonical name used in the 503 feature_disabled
// body when a WRITE is attempted before Core file storage is configured.
const FeatureCoreStorage = "core_storage"

// FeatureBYON is the canonical name used in X-Feature-Disabled headers + JSON
// error responses for the bring-your-own-node multi-tenancy subsystem.
const FeatureBYON = "byon"

// FeatureStore is the canonical name for the hosted store link (STORE_URL +
// STORE_SHARED_KEY).
const FeatureStore = "store"

// RequireModpacksEnabled blocks the request with 503 feature_disabled when
// the platform-wide modpacks toggle is off. Use on every WRITE endpoint that
// touches modpack data (modpacks CRUD, versions, mods, publish, mrpack PAT
// management).
func (s *AppState) RequireModpacksEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.FeatureFlags.IsModpacksEnabled(r.Context()) {
			featureDisabledResponse(w, FeatureModpacks, "Modpack authoring is disabled by the platform admin.")
			return
		}
		next(w, r)
	}
}

// RequireShareLinksEnabled blocks share-link CREATION with 503 feature_disabled
// when the admin has not enabled share links. Layer it between
// RequireModpacksEnabled and RequireUserCanCreateModpacks.
func (s *AppState) RequireShareLinksEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.FeatureFlags.IsShareLinksEnabled(r.Context()) {
			featureDisabledResponse(w, FeatureShareLinks, "Share links are disabled by the platform admin.")
			return
		}
		next(w, r)
	}
}

// RequireTicketsEnabled blocks the request with 503 feature_disabled when
// the platform-wide tickets toggle is off. Wrap every endpoint under the
// ticket subsystem (tickets, categories, attachments, canned responses,
// notifications, settings) so the panel + external clients all see the same
// 503 when the admin pauses tickets.
func (s *AppState) RequireTicketsEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.FeatureFlags.IsTicketsEnabled(r.Context()) {
			featureDisabledResponse(w, FeatureTickets, "The ticket system is disabled by the platform admin.")
			return
		}
		next(w, r)
	}
}

// RequireBYONEnabled blocks the request with 503 feature_disabled when the
// platform-wide BYON toggle is off. Wrap the ADMIN usage/billing/plans routes
// so they refuse cleanly while the platform runs as today's single-operator
// panel. Additive to the plans.* capability checks already on those routes.
func (s *AppState) RequireBYONEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.FeatureFlags.IsBYONEnabled(r.Context()) {
			featureDisabledResponse(w, FeatureBYON, "BYON (bring-your-own-node) is disabled by the platform admin.")
			return
		}
		next(w, r)
	}
}

// RequireStoreEnabled blocks the request with 503 feature_disabled when this
// install is not linked to the hosted store.
//
// For the endpoints that set money-shaped policy: platform billing defaults and
// the per-user overrides. Without a store nothing was ever bought, so those
// numbers govern nobody - and the panel has always hidden their screen on
// `requiresStore`, so the API was answering for a page the operator could not
// open. The billing STATUS route deliberately keeps only the BYON gate: an
// operator suspending a user by hand is a thing a self-hosted install does.
//
// StoreEnabled is process config (config.StoreEnabled), not a settings row, so
// this reads a field rather than the feature-flag cache.
func (s *AppState) RequireStoreEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.StoreEnabled {
			featureDisabledResponse(w, FeatureStore,
				"This install is not linked to the hosted store, so there is nothing to bill. Per-user backup allowances live under Settings, Backups.")
			return
		}
		next(w, r)
	}
}

// RequireCoreStorageConfigured blocks a WRITE with 503 feature_disabled when no
// valid Core file storage config exists. Wrap every endpoint that STORES a blob
// (library upload/mkdir, ticket attachment upload, ticket backup create).
func (s *AppState) RequireCoreStorageConfigured(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.CoreStorageConfigured() {
			featureDisabledResponse(w, FeatureCoreStorage,
				"Configure Core file storage (Settings -> Core file storage) before writing files.")
			return
		}
		next(w, r)
	}
}

// RequireCoreStorageReachable blocks a request with 503 when THIS Core's own
// self-check says it cannot use the shared core file storage.
//
// This is the surgical route-gate: the Core stays up and keeps serving auth,
// server management and the panel, so an operator can still SEE the fault. A
// hard crash or a failing /healthz would take the control plane down with the
// storage and leave nobody able to read the error.
//
// Wrap it around every route whose handler builds a core storage provider.
func (s *AppState) RequireCoreStorageReachable(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.StorageReach == nil || s.StorageReach.Status().Healthy() {
			next(w, r)
			return
		}
		// The second Snapshot() value is the raw backend error text (mount
		// paths, bucket names, S3 endpoints); five of the fourteen gated
		// routes (routes.go: the three ticket-attachment routes, plus
		// GET /api/library and GET /api/library/download) are open to any
		// authenticated user with no capability check, so it must never
		// reach this body. The taxonomy value + remedy below are what a
		// caller needs; the full detail stays on the fault-list endpoint for
		// admins.
		status, _ := s.StorageReach.Status().Snapshot()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "storage_unreachable",
			"reason":  string(status),
			"message": storageReachMessage(status),
		})
	}
}

// storageReachMessage is the operator-facing remedy for each taxonomy value.
// Every Status must have one: an empty body on a 503 is the silent failure
// this whole feature exists to remove.
func storageReachMessage(status storagereach.Status) string {
	switch status {
	case storagereach.StatusUnreachable:
		return "This Core cannot reach the shared file storage. Check that the mount is present on this host, or that the S3 endpoint is reachable from it."
	case storagereach.StatusWriteDenied:
		// uid 1000 is named explicitly because it is the most likely cause on a
		// filesystem path and the least discoverable: Core runs as a non-root
		// user, so a share that root can write is still refused. "Credentials"
		// alone pointed the operator at S3 even when the backend was a mount.
		return "This Core can reach the shared file storage but cannot write to it. On a filesystem path, check that the mount is not read-only and that the path is writable by uid 1000, the non-root user Core runs as; pointing the config at a subdirectory you chowned to 1000 also works. On S3, check that the credentials allow writes."
	case storagereach.StatusNotShared:
		return "This Core cannot see files written by the other Cores, so the storage is not actually shared. Point every Core at the same S3 bucket, or mount the same filesystem at this path on every host."
	case storagereach.StatusFingerprintMismatch:
		return "This Core is pointed at a different storage backend than the rest of the deployment. Re-save the Core file storage settings so every Core agrees."
	case storagereach.StatusCrossWriteDenied:
		return "This Core cannot write into files owned by the other Cores. Check the share's user mapping (NFS root_squash / uid mapping) so all Cores write as the same user."
	case storagereach.StatusOffline, storagereach.StatusNoResponse:
		return "This Core did not confirm access to the shared file storage."
	default:
		return "This Core cannot currently use the shared file storage."
	}
}

// AllowReadOnlyWhenDisabled lets the request through but sets the
// X-Feature-Disabled response header when the toggle is off. Used on READ
// endpoints (list/get modpacks, download .mrpack) so existing modpacks stay
// viewable + downloadable when the feature is paused.
func (s *AppState) AllowReadOnlyWhenDisabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.FeatureFlags.IsModpacksEnabled(r.Context()) {
			w.Header().Set("X-Feature-Disabled", FeatureModpacks)
		}
		next(w, r)
	}
}

// RequireUserCanCreateModpacks gates END-USER authoring: the platform-wide
// authoring flag AND the caller's per-user can_create_modpacks. Admins bypass
// both, which is what makes "modpacks on, authoring off" mean admins-only.
//
// The flag is checked BEFORE the per-user column on purpose. It is a ceiling,
// not a default: a user whose flag was set to true by hand (and is therefore
// marked manual, so a bulk apply leaves it alone) must still lose authoring when
// the admin closes it for everyone. Checking only the column would let those
// rows keep authoring after the global switch went off.
//
// Layer this AFTER RequireModpacksEnabled so the user lookup only happens when
// the subsystem check passes.
func (s *AppState) RequireUserCanCreateModpacks(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isAdmin, _ := r.Context().Value("isAdmin").(bool); isAdmin {
			next(w, r)
			return
		}
		if !s.FeatureFlags.IsModpackAuthoringEnabled(r.Context()) {
			featureDisabledResponse(w, FeatureModpackAuthoring, "Modpack authoring is not open to users on this platform.")
			return
		}
		userID, _ := r.Context().Value("userID").(string)
		if userID == "" {
			sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		user, err := s.Store.GetUserByID(userID)
		if err != nil || user == nil {
			sendJSONError(w, "User not found", http.StatusUnauthorized)
			return
		}
		if !user.CanCreateModpacks {
			featureDisabledResponse(w, "modpacks_user", "Your account is not permitted to author modpacks.")
			return
		}
		next(w, r)
	}
}

// featureDisabledResponse writes the canonical 503 + X-Feature-Disabled +
// machine-readable error body.
func featureDisabledResponse(w http.ResponseWriter, feature, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Feature-Disabled", feature)
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   "feature_disabled",
		"feature": feature,
		"message": msg,
	})
}
