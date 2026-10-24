package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

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

func SendJSONError(w http.ResponseWriter, message string, code int) {
	sendJSONError(w, message, code)
}

func Slugify(name string) string { return slugify(name) }

func GeneratePlaintextKey() (string, error) { return generatePlaintextKey() }

func CapBody(w http.ResponseWriter, r *http.Request, max int64) bool { return capBody(w, r, max) }

type ModpackDelivery = modpackDelivery

const (
	DeliverStream   = deliverStream
	DeliverRedirect = deliverRedirect
)

func ServeModpackObject(w http.ResponseWriter, r *http.Request, prov modpack.ModpackStorageProvider, key string, mode ModpackDelivery, contentType, filename string) error {
	return serveModpackObject(w, r, prov, key, mode, contentType, filename)
}

func CompatQuery(r *http.Request) (mode, specific string) { return compatQuery(r) }

func ResolveTargets(ctx context.Context, current, mode, specific string) ([]string, error) {
	return resolveTargets(ctx, current, mode, specific)
}

func (s *AppState) AvailabilityCache() services.AvailabilityCache { return s.availabilityCache() }

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

func ParseIntDefault(v string, def int) int { return parseIntDefault(v, def) }

func ClientIP(r *http.Request) string { return clientIP(r) }

func ParseUserID(w http.ResponseWriter, r *http.Request, varName ...string) (string, bool) {
	return parseUserID(w, r, varName...)
}

func MatchTOTPStep(code, secret string, now time.Time) (step int64, ok bool) {
	return matchTOTPStep(code, secret, now)
}

func CoreStorageUnavailableResponse(w http.ResponseWriter, err error) {
	coreStorageUnavailableResponse(w, err)
}

func SendConnTestFailure(w http.ResponseWriter, check services.ConnCheck) {
	sendConnTestFailure(w, check)
}

func SetAttachmentDisposition(w http.ResponseWriter, filename string) {
	setAttachmentDisposition(w, filename)
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

const StatsStreamsPerUser = statsStreamsPerUser

func AcquireLiveStream(w http.ResponseWriter, r *http.Request, st store.Store, kind string, max int) (release func(), ok bool) {
	return acquireLiveStream(w, r, st, kind, max)
}

func StreamContext(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc) {
	return streamContext(w, r)
}

// SSERetryLine opens every SSE stream; exported for the subpackages' tests.
const SSERetryLine = sseRetryLine

// SetSSEMaxStreamAge changes how long an SSE stream lives before it ends and
// the client reconnects, and returns the function that restores the previous
// value. For tests: production never changes it.
func SetSSEMaxStreamAge(d time.Duration) (restore func()) {
	prev := sseMaxStreamAge
	sseMaxStreamAge = d
	return func() { sseMaxStreamAge = prev }
}

func SendJSONErrorCode(w http.ResponseWriter, message, code string, status int) {
	sendJSONErrorCode(w, message, code, status)
}

func SettingOrUnset(st store.Store, key string) (string, error) { return settingOrUnset(st, key) }

func RefuseIfSuspended(w http.ResponseWriter, r *http.Request, state *AppState, srv *models.Server) bool {
	return refuseIfSuspended(w, r, state, srv)
}

func RefuseIfServerBusy(w http.ResponseWriter, r *http.Request, state *AppState, srv *models.Server) bool {
	return refuseIfServerBusy(w, r, state, srv)
}

func PurgeBackupArchivesForJob(state *AppState, jobID int) { purgeBackupArchivesForJob(state, jobID) }

func CoreStorageConfigFromConnection(conn *models.StorageConnection) CoreStorageConfig {
	return coreStorageConfigFromConnection(conn)
}

func AnnotateStalledRestores(ctx context.Context, rdb *redis.Client, connected func(nodeID int) bool, nodeID int, serverUUID string, restores []models.BackupRestore) {
	annotateStalledRestores(ctx, rdb, connected, nodeID, serverUUID, restores)
}

func ValidateS3Endpoint(subject, endpoint string) error { return validateS3Endpoint(subject, endpoint) }

// PathIsOnContainerRootFS is the real check behind the root-filesystem seams;
// a subpackage keeps its own seam variable pointing here so its tests can
// replace it without touching this package's.
func PathIsOnContainerRootFS(path string) (onRoot bool, determinable bool) {
	return pathIsOnContainerRootFS(path)
}

func ValidateTabURL(raw string) error { return validateTabURL(raw) }

// SessionClaimsKey is the request-context key the auth middleware stores the
// session's claims under. The value itself, not a copy: a subpackage reading
// the context has to ask with the identical key.
const SessionClaimsKey = sessionClaimsKey

const TabProxyTicketTTL = tabProxyTicketTTL

type SparkDBProvider = sparkDBProvider

func SyncCustomTabsModule(state *AppState, enabled bool, audience string) error {
	return syncCustomTabsModule(state, enabled, audience)
}

func IsDemoAccount(s *AppState, userID string) bool { return isDemoAccount(s, userID) }

func GuardAccountTerms(w http.ResponseWriter, r *http.Request, state *AppState, userID string) bool {
	return guardAccountTerms(w, r, state, userID)
}

func ByonCallerID(r *http.Request) string { return byonCallerID(r) }

func ValidatePublicBaseURL(subject, raw string) error { return validatePublicBaseURL(subject, raw) }

func UserRouteScope(userID string) string { return userRouteScope(userID) }

func UsagePeriod(r *http.Request) time.Time { return usagePeriod(r) }

func (s *AppState) ProbeStoreLink(ctx context.Context, uuid string) (bool, string, error) {
	return s.probeStoreLink(ctx, uuid)
}

func (s *AppState) StoreAccountSummary(ctx context.Context, uuid string) (map[string]interface{}, error) {
	return s.storeAccountSummary(ctx, uuid)
}

func (s *AppState) StoreSetBillingConsent(ctx context.Context, uuid string, traffic, backup *bool) (int, string, error) {
	return s.storeSetBillingConsent(ctx, uuid, traffic, backup)
}

func ResolveRelay(ctx context.Context, rdb *redis.Client, manualOverride, publicHost, preferredRegion string) (string, string) {
	return resolveRelay(ctx, rdb, manualOverride, publicHost, preferredRegion)
}

func IsDemoAccountChecked(s *AppState, userID string) (bool, error) {
	return isDemoAccountChecked(s, userID)
}

func RedactNodeAddressOne(state *AppState, srv *models.Server, isAdmin bool, userID string) {
	redactNodeAddressOne(state, srv, isAdmin, userID)
}

// SuspendedMessage is what a suspended account is told; for the subpackages'
// tests that assert it.
const SuspendedMessage = suspendedMessage

// DemoAccountUUIDSetting is the settings key naming the demo account.
const DemoAccountUUIDSetting = demoAccountUUIDSetting
