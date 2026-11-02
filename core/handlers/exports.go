package handlers

import (
	"context"
	"net/http"
	"time"

	"dylaris-core/handlers/reauth"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage"
	"dylaris-core/storage/modpack"
	"dylaris-core/store"
)

// Exported for the handlers/<domain> subpackages (packs, tickets, ...), which
// live in their own packages so that a change there does not recompile all of
// handlers. Thin wrappers rather than renames: the unexported names stay what
// the rest of this package calls.

func (s *AppState) BuildModpackStorageProvider() (modpack.ModpackStorageProvider, error) {
	return s.buildModpackStorageProvider()
}

func (s *AppState) PackBuildDirs(pack *models.Pack, build *models.PackBuild) []string {
	return s.packBuildDirs(pack, build)
}

func (s *AppState) PackDirsOfPack(pack *models.Pack) []string { return s.packDirsOfPack(pack) }

func (s *AppState) BuildCoreStorageProvider(subPrefix string) (storage.StorageProvider, error) {
	return s.buildCoreStorageProvider(subPrefix)
}

// MayReceive reports whether the caller of r may see a system event. Exported
// so the packs tests can prove their events reach only the pack owner.
func (h *SystemEventsHandler) MayReceive(r *http.Request, payload string) bool {
	return h.mayReceive(r, payload)
}

func CoreStorageUnavailableResponse(w http.ResponseWriter, err error) {
	coreStorageUnavailableResponse(w, err)
}

func SendConnTestFailure(w http.ResponseWriter, check services.ConnCheck) {
	sendConnTestFailure(w, check)
}

// Core storage setting keys, for the subpackages' tests that configure a store.
const (
	KeyCoreStorageBackend     = keyCoreStorageBackend
	KeyCoreStoragePath        = keyCoreStoragePath
	KeyCoreStoragePathConfirm = keyCoreStoragePathConfirm
	KeyCoreStorageS3Endpoint  = keyCoreStorageS3Endpoint
	KeyCoreStorageS3Bucket    = keyCoreStorageS3Bucket
	KeyCoreStorageS3AccessKey = keyCoreStorageS3AccessKey
	KeyCoreStorageS3SecretKey = keyCoreStorageS3SecretKey
)

func SettingOrUnset(st store.Store, key string) (string, error) { return settingOrUnset(st, key) }

func PurgeBackupArchivesForJob(state *AppState, jobID int) { purgeBackupArchivesForJob(state, jobID) }

func CoreStorageConfigFromConnection(conn *models.StorageConnection) CoreStorageConfig {
	return coreStorageConfigFromConnection(conn)
}

// --- gateway -----------------------------------------------------------------

func (s *AppState) GatewayEnabled() bool { return s.gatewayEnabled() }

func GatewayDisabledResponse(w http.ResponseWriter) { gatewayDisabledResponse(w) }

// --- nodes -------------------------------------------------------------------

func PurgeBackupArchivesForServers(state *AppState, serverIDs []int) {
	purgeBackupArchivesForServers(state, serverIDs)
}

// --- mods --------------------------------------------------------------------

const KeyModpackStorageConnectionID = keyModpackStorageConnectionID

func StorageConnIDSetting(id int) string { return storageConnIDSetting(id) }

// --- storage -----------------------------------------------------------------

func SendReachRefusal(w http.ResponseWriter, err error) { sendReachRefusal(w, err) }

type CoreStorageSettingWriteError = coreStorageSettingWriteError

func ValidateCoreStorageConfig(c CoreStorageConfig) error { return validateCoreStorageConfig(c) }

func (s *AppState) CheckSharedStorageReachable(ctx context.Context, cfg CoreStorageConfig) error {
	return s.checkSharedStorageReachable(ctx, cfg)
}

func (s *AppState) PersistCoreStorageConfig(cfg CoreStorageConfig, secret string) error {
	return s.persistCoreStorageConfig(cfg, secret)
}

func NewStorageProviderForConfig(cfg CoreStorageConfig, subPrefix string, gate *storage.Gate, s3res *storage.S3Resilience) (storage.StorageProvider, error) {
	return newStorageProviderForConfig(cfg, subPrefix, gate, s3res)
}

func NormalizeCoreStorageCandidate(c CoreStorageConfig) CoreStorageConfig {
	return normalizeCoreStorageCandidate(c)
}

func MergeCoreStorageCandidate(candidate, existing CoreStorageConfig) CoreStorageConfig {
	return mergeCoreStorageCandidate(candidate, existing)
}

func ProbeStorageProvider(ctx context.Context, prov storage.StorageProvider) (ok bool, message string) {
	return probeStorageProvider(ctx, prov)
}

func CoreStorageConfigFromTarget(t services.StorageTargetConfig) CoreStorageConfig {
	return coreStorageConfigFromTarget(t)
}

func CoreStorageRoot(cfg CoreStorageConfig, subPrefix string) string {
	return coreStorageRoot(cfg, subPrefix)
}

func SameCoreStorageLocation(src, tgt CoreStorageConfig, srcSubPrefix, tgtSubPrefix string) (bool, error) {
	return sameCoreStorageLocation(src, tgt, srcSubPrefix, tgtSubPrefix)
}

func EnsureDistinctCoreStorageLocation(src, tgt CoreStorageConfig, srcSubPrefix, tgtSubPrefix string) error {
	return ensureDistinctCoreStorageLocation(src, tgt, srcSubPrefix, tgtSubPrefix)
}

func BuildTargetStorageProvider(cfg CoreStorageConfig, subPrefix string) (storage.StorageProvider, error) {
	return buildTargetStorageProvider(cfg, subPrefix)
}

type StorageConnectionConfig = storageConnectionConfig

func StorageConnectionProvider(conn *models.StorageConnection) (storage.StorageProvider, error) {
	return storageConnectionProvider(conn)
}

// SetReachRoundDeadline and SetReachRoundPoll shorten the shared-storage
// verification round for the subpackages' tests (see the fields in state.go).
// Production never calls them.
func (s *AppState) SetReachRoundDeadline(d time.Duration) { s.reachRoundDeadline = d }

func (s *AppState) SetReachRoundPoll(d time.Duration) { s.reachRoundPoll = d }

const (
	KeyCoreStorageS3Region     = keyCoreStorageS3Region
	KeyCoreStorageConnectionID = keyCoreStorageConnectionID
)

// --- users -------------------------------------------------------------------

// AdminReauthRequest is embedded in the admin account-action bodies; the alias
// keeps the JSON shape ({"reauth": {...}}) exactly as it was.
type ()

func NotifyPasswordChanged(state *AppState, email, username string) {
	notifyPasswordChanged(state, email, username)
}

// SetAccountMailSender replaces the account notice sender for a test and
// returns the restore func. Production never calls it.
func SetAccountMailSender(fn func(services.MailStore, string, string, string, map[string]string) error) (restore func()) {
	prev := sendAccountMail
	sendAccountMail = fn
	return func() { sendAccountMail = prev }
}

var DefaultPlacementSettings = defaultPlacementSettings

// ReauthCodeVerifier is the second-factor check re-authentication hands to
// reauth.RequireAdminReauth / RequireReauth, for the subpackages.
func ReauthCodeVerifier(state *AppState) reauth.CodeVerifier { return reauthCodeVerifier(state) }
