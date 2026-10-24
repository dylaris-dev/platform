package handlers

import (
	"context"
	"net/http"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage"
	"dylaris-core/storage/modpack"
)

// Exported for handlers/packs, which lives in its own package so that a change
// to a pack handler does not recompile all of handlers. Thin wrappers rather
// than renames: the unexported names stay what the rest of this package calls.

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
