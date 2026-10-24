package handlers

import (
	"context"
	"net/http"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage"
	"dylaris-core/storage/modpack"
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
