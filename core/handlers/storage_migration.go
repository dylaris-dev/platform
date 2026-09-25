package handlers

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/storagemigrate"
	"dylaris-core/storage"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/storage/modpack"

	"github.com/gorilla/mux"
)

// ModpacksCoreStorageDataSetID names the modpacks namespace INSIDE the shared
// Core file storage, as distinct from "modpacks", which is whatever the
// modpack_storage_* settings currently point at. The pair is what makes an
// automated modpacks migration expressible: source "modpacks", target
// "modpacks@core-storage".
const ModpacksCoreStorageDataSetID = "modpacks@core-storage"

// CoreStorageDataSetID names the shared Core file storage AS A WHOLE - every
// key under its root, which is library/, ticket-attachments/ and
// ticket-backups/ together (plus server-backups/ and modpacks/ for an install
// that has pointed those at Core storage too).
//
// It exists because those namespaces do not have a config each: they all read
// ONE saved Core file storage config, and switching it repoints all of them at
// once. Offering a per-namespace config switch therefore stranded the others -
// migrating library alone and switching left ticket-attachments and
// ticket-backups pointing at a backend their data had never been copied to,
// and, because their source config then named that new empty backend, the
// operator could not even name the old location to migrate them afterwards.
// One data set covering the whole root is the only config switch that is safe
// to offer for this group; the individual ones keep manifest capture and
// verification for the manual flow.
const CoreStorageDataSetID = "core-storage"

// coreStorageBackendLabel renders a CREDENTIAL-FREE description of where a
// Core-file-storage-scoped data set lives. Safety invariant 6: labels are
// endpoint + bucket + prefix at most, never keys. Every label that reaches a
// manifest, a job record, a log line or a CSV export comes from here or from
// backupStorageLabel. An empty subPrefix means the Core storage root itself.
func coreStorageBackendLabel(cfg CoreStorageConfig, subPrefix string) string {
	if cfg.Backend == "s3" {
		var parts []string
		if cfg.S3Prefix != "" {
			parts = append(parts, strings.Trim(cfg.S3Prefix, "/"))
		}
		if subPrefix != "" {
			parts = append(parts, subPrefix)
		}
		// Through the sanitizer, NOT a raw concatenation. An s3 endpoint can
		// carry embedded userinfo (https://AKIA...:secret@minio.internal), and
		// this label is persisted into storage_manifests.backend_label, the
		// Redis job record, the panel job log and the manifest CSV export.
		// validateCoreStorageConfig rejects such an endpoint at the boundary;
		// this is the second half, so the guarantee holds for any config that
		// predates or bypasses that check rather than resting on it.
		//
		// TrimSuffix before the call preserves the historic rendering of an
		// endpoint written with a trailing slash; it cannot introduce userinfo.
		// TrimSuffix after it drops the empty trailing prefix segment for a
		// data set that has neither a configured prefix nor a sub-prefix.
		return strings.TrimSuffix(
			storagemigrate.SanitizeBackendLabel(strings.TrimSuffix(cfg.S3Endpoint, "/"), cfg.S3Bucket, strings.Join(parts, "/")),
			"/")
	}
	root := strings.TrimSuffix(cfg.Path, "/")
	if subPrefix == "" {
		return "path:" + root
	}
	return "path:" + root + "/" + subPrefix
}

// backupStorageLabel renders a credential-free description of a
// backup_storages row. The row's Config is NEVER included: it holds bucket
// credentials for the s3 provider.
func backupStorageLabel(bs models.BackupStorage) string {
	return fmt.Sprintf("%s (%s)", bs.Name, bs.Provider)
}

// StorageDataSetResolver turns a data-set id into a live DataSet plus its
// label. It lives in handlers because it needs the Core file storage config,
// the modpack settings and the backup_storages rows - all of which handlers
// already owns.
type StorageDataSetResolver struct {
	state *AppState
}

func NewStorageDataSetResolver(state *AppState) *StorageDataSetResolver {
	return &StorageDataSetResolver{state: state}
}

// adHocTargetNote tells the operator how a data set with a backend of its own
// (today: modpacks, and the whole Core file storage) names a migrate target.
// Its SAVED config says where it lives now, so it cannot also be the
// destination; the destination is supplied inline instead.
// The delete wording is deliberately scoped to the manifest rather than to
// "the old copy": the engine deletes exactly the keys the manifest names, so
// post-capture writes survive and, for modpacks, storage objects no database
// row references were never enumerated in the first place.
const adHocTargetNote = "Migrating this data set means naming a new storage config (another S3, or a mounted path) in the wizard. The copy is verified, the active config is switched to the target only after that verification passes, and the objects named in the manifest are deleted from the source only if you opt in. The manual flow (capture a manifest, move the data yourself, reconfigure, verify) is still available."

// combinedCoreStorageNote describes what the whole-Core-file-storage data set
// covers and why it is the one that owns the config switch.
const combinedCoreStorageNote = "Covers the whole Core file storage in one move: Library, ticket attachments and ticket backups (and server backups / modpacks for an install that keeps those on Core storage too). " + adHocTargetNote

// sharedCoreStorageNote is carried by each namespace INSIDE the Core file
// storage. They share one saved config, so none of them can switch it alone
// without stranding the others - see CoreStorageDataSetID.
const sharedCoreStorageNote = "This is one namespace inside the shared Core file storage, and all of them read a single saved config, so it cannot be moved to a new backend on its own. Migrate the \"" + CoreStorageDataSetID + "\" data set to move the whole Core file storage automatically. Capturing a manifest and verifying this namespace on its own still works, for the manual flow."

// nodeLocalNote explains why node-local backup rows are excluded.
const nodeLocalNote = "Node-local archives live on Node disks and are reachable only through the gRPC mesh, so they are not migratable here."

// modpackOrphanNote is surfaced next to every modpacks verdict.
const modpackOrphanNote = "Modpack keys are enumerated from the database, so verification covers database-referenced objects only. An object in storage that no row points at is invisible to this check."

// List describes every data set for the overview.
func (r *StorageDataSetResolver) List(_ context.Context) ([]services.StorageDataSetInfo, error) {
	cfg := r.state.LoadCoreStorageConfig()
	out := []services.StorageDataSetInfo{
		// The combined one first: it is the only member of this group that can
		// switch the shared config, so it is the answer to "move my Core file
		// storage somewhere else".
		{ID: CoreStorageDataSetID, Label: "Core file storage (all namespaces)", BackendLabel: coreStorageBackendLabel(cfg, ""), Migratable: true, SupportsTargetConfig: true, Note: combinedCoreStorageNote},
		{ID: storagemigrate.DataSetLibrary, Label: "Library", BackendLabel: coreStorageBackendLabel(cfg, CoreStoragePrefixLibrary), Migratable: true, SupportsTargetConfig: false, Note: sharedCoreStorageNote},
		{ID: storagemigrate.DataSetAttachments, Label: "Ticket attachments", BackendLabel: coreStorageBackendLabel(cfg, CoreStoragePrefixAttachments), Migratable: true, SupportsTargetConfig: false, Note: sharedCoreStorageNote},
		{ID: storagemigrate.DataSetTicketBackups, Label: "Ticket backups", BackendLabel: coreStorageBackendLabel(cfg, CoreStoragePrefixBackups), Migratable: true, SupportsTargetConfig: false, Note: sharedCoreStorageNote},
		// Modpacks keep an ad-hoc target: modpack_storage_* is their OWN
		// settings namespace, so switching it strands nothing else.
		{ID: storagemigrate.DataSetModpacks, Label: "Modpacks", BackendLabel: r.modpackBackendLabel(), Migratable: true, SupportsTargetConfig: true, Note: modpackOrphanNote},
		// ...but the modpacks namespace INSIDE Core storage is governed by the
		// shared config like the three above, so it is a migrate TARGET only.
		{ID: ModpacksCoreStorageDataSetID, Label: "Modpacks on Core file storage", BackendLabel: coreStorageBackendLabel(cfg, CoreStoragePrefixModpacks), Migratable: true, SupportsTargetConfig: false, Note: modpackOrphanNote + " " + sharedCoreStorageNote},
	}

	storages, err := r.state.Store.ListBackupStorages()
	if err != nil {
		return nil, fmt.Errorf("list backup storages: %w", err)
	}
	for _, bs := range storages {
		info := services.StorageDataSetInfo{
			ID:           storagemigrate.ServerBackupsDataSetID(bs.ID),
			Label:        "Server backups: " + bs.Name,
			BackendLabel: backupStorageLabel(bs),
			Migratable:   bs.Provider != "node-local",
			// Backups are multi-storage by design (backup_storages rows +
			// BackupJob.StorageID), which is strictly richer than one global
			// config, so their target stays another ROW rather than an ad-hoc
			// config. That model is untouched by this workstream.
			SupportsTargetConfig: false,
		}
		if bs.Provider == "node-local" {
			info.Note = nodeLocalNote
		}
		out = append(out, info)
	}
	return out, nil
}

// modpackConfiguredPaths parses the modpack_storage_paths setting. It is a
// JSON array (see storage/modpack.NewProviderFromSettings and
// ModpackSettingsHandler.Set, which writes json.Marshal(cleaned)), NOT a
// comma-separated list, so this must go through json.Unmarshal rather than
// strings.Split - a comma-split would hand back the literal bracketed/quoted
// JSON text as a "path", which would never match a real root and would
// silently defeat sameCoreStorageLocation's same-location refusal for the
// modpacks data set (a garbled source path can never compare equal to any
// real target, so a genuine alias would sail through the check instead of
// being refused).
func (r *StorageDataSetResolver) modpackConfiguredPaths() []string {
	raw, _ := r.state.Store.GetSetting("modpack_storage_paths")
	var paths []string
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &paths)
	}
	return paths
}

// modpackBackendLabel describes the CURRENT modpack backend without touching
// its credentials.
func (r *StorageDataSetResolver) modpackBackendLabel() string {
	get := func(k string) string {
		v, _ := r.state.Store.GetSetting(k)
		return v
	}
	switch get("modpack_storage_provider") {
	case "s3":
		// Same sanitizer as coreStorageBackendLabel: the modpack s3 endpoint is
		// operator-supplied too, and this label reaches the same manifest rows,
		// job records and CSV exports.
		return strings.TrimSuffix(
			storagemigrate.SanitizeBackendLabel(strings.TrimSuffix(get("modpack_storage_s3_endpoint"), "/"), get("modpack_storage_s3_bucket"), ""),
			"/")
	case "core-storage":
		return coreStorageBackendLabel(r.state.LoadCoreStorageConfig(), CoreStoragePrefixModpacks)
	default:
		return "path:" + strings.Join(r.modpackConfiguredPaths(), ",")
	}
}

// Resolve builds a live DataSet. Providers resolve PER CALL, never cached.
func (r *StorageDataSetResolver) Resolve(ctx context.Context, id string) (storagemigrate.DataSet, string, error) {
	cfg := r.state.LoadCoreStorageConfig()

	providerSet := func(subPrefix, label string) (storagemigrate.DataSet, string, error) {
		prov, err := r.state.buildCoreStorageProvider(subPrefix)
		if err != nil {
			return nil, "", err
		}
		return storagemigrate.NewProviderDataSet(id, label, prov), coreStorageBackendLabel(cfg, subPrefix), nil
	}

	switch id {
	case CoreStorageDataSetID:
		// An EMPTY sub-prefix, so the provider is rooted at the Core storage
		// root and WalkProvider enumerates every namespace under it
		// ("library/x.jar", "ticket-attachments/...", ...). Writing those same
		// keys through a target provider that is also rooted at ITS root
		// reproduces the layout exactly, which is what makes one config switch
		// correct for all of them at once.
		return providerSet("", "Core file storage")
	case storagemigrate.DataSetLibrary:
		return providerSet(CoreStoragePrefixLibrary, "Library")
	case storagemigrate.DataSetAttachments:
		return providerSet(CoreStoragePrefixAttachments, "Ticket attachments")
	case storagemigrate.DataSetTicketBackups:
		return providerSet(CoreStoragePrefixBackups, "Ticket backups")

	case storagemigrate.DataSetModpacks:
		prov, err := modpack.NewProviderFromSettings(r.state.Store.GetSetting, r.state.buildCoreStorageProvider)
		if err != nil {
			return nil, "", fmt.Errorf("modpack storage: %w", err)
		}
		if prov == nil {
			return nil, "", errors.New("modpack storage is not configured")
		}
		return storagemigrate.NewModpackDataSet(id, "Modpacks", prov, r.state.Store), r.modpackBackendLabel(), nil

	case ModpacksCoreStorageDataSetID:
		p, err := r.state.buildCoreStorageProvider(CoreStoragePrefixModpacks)
		if err != nil {
			return nil, "", err
		}
		return storagemigrate.NewModpackDataSet(id, "Modpacks on Core file storage", modpack.NewCoreStorageProvider(p), r.state.Store),
			coreStorageBackendLabel(cfg, CoreStoragePrefixModpacks), nil
	}

	if storageID, ok := storagemigrate.ParseServerBackupsDataSetID(id); ok {
		bs, err := r.state.Store.GetBackupStorage(storageID)
		if err != nil || bs == nil {
			return nil, "", fmt.Errorf("backup storage %d not found", storageID)
		}
		if bs.Provider == "node-local" {
			return nil, "", errors.New(nodeLocalNote)
		}
		st, err := backupstorage.Open(ctx, bs, r.backupDeps())
		if err != nil {
			return nil, "", fmt.Errorf("open backup storage %d: %w", storageID, err)
		}
		ds, err := storagemigrate.NewBackupDataSet(id, "Server backups: "+bs.Name, st)
		if err != nil {
			return nil, "", err
		}
		return ds, backupStorageLabel(*bs), nil
	}
	return nil, "", fmt.Errorf("unknown data set %q", id)
}

// storageSourceLocation is where a data set's objects physically live today,
// plus where an ad-hoc migration target will put them.
//
// The two sub-prefixes exist because they are NOT always equal. The target of
// every config-switchable data set is a Core-storage-shaped provider, so
// tgtSubPrefix is that data set's namespace under the target root. The SOURCE,
// though, is whatever its own backend does, and modpack.LocalProvider writes at
// filepath.Join(base, key) with no sub-directory at all - its root IS
// modpack_storage_paths[0]. Comparing with one shared sub-prefix therefore
// compared "<paths[0]>/modpacks" against "<target>/modpacks" and judged
// paths[0]="/data/modpacks" distinct from a "/data" target that resolves to
// precisely that directory: the copy would rewrite every object onto itself, a
// full verify would pass (comparing each file with itself), the switch would
// succeed and an opted-in deleteSource would then remove the only copy.
type storageSourceLocation struct {
	cfg          CoreStorageConfig
	srcSubPrefix string
	tgtSubPrefix string
}

// sourceCoreStorageConfigFor returns the SOURCE data set's effective config and
// sub-prefixes, for the same-location comparison and the labels.
//
// It accepts only data sets that OWN a settings-configured backend, because
// accepting one means offering to repoint that backend after the copy:
//
//   - CoreStorageDataSetID: the whole Core file storage, one config, one root.
//   - modpacks: its own modpack_storage_* namespace.
//
// The namespaces INSIDE the Core file storage (library, ticket-attachments,
// ticket-backups, modpacks@core-storage) are refused. They have no config of
// their own - they all read the shared one - so switching on behalf of any one
// of them would repoint the others onto a backend their data was never copied
// to, and leave them unrecoverable: their source config would then name that
// new empty backend, so the old location could not even be named as a source
// afterwards.
//
// A server-backups row is refused too: those name a target ROW, not a config.
//
// For modpacks the config is a CoreStorageConfig-SHAPED VIEW of the
// modpack_storage_* settings, built for the comparison and the label only. It
// is never persisted, and SwitchConfig for modpacks writes the modpack
// settings, not the Core file storage ones.
func (r *StorageDataSetResolver) sourceCoreStorageConfigFor(id string) (storageSourceLocation, error) {
	switch id {
	case CoreStorageDataSetID:
		return storageSourceLocation{cfg: r.state.LoadCoreStorageConfig()}, nil
	case storagemigrate.DataSetModpacks:
		return storageSourceLocation{
			cfg:          r.modpackSourceConfig(),
			srcSubPrefix: r.modpackSourceSubPrefix(),
			tgtSubPrefix: CoreStoragePrefixModpacks,
		}, nil
	case storagemigrate.DataSetLibrary, storagemigrate.DataSetAttachments,
		storagemigrate.DataSetTicketBackups, ModpacksCoreStorageDataSetID:
		return storageSourceLocation{}, fmt.Errorf(
			"data set %q is one namespace inside the shared Core file storage and cannot switch that config on its own (it would strand the others); migrate %q to move the whole Core file storage automatically, or use the manual flow for this namespace",
			id, CoreStorageDataSetID)
	}
	return storageSourceLocation{}, fmt.Errorf("data set %q does not take a storage config as its target; pick another data set instead", id)
}

// modpackSourceSubPrefix is the sub-directory (or key prefix) the CURRENT
// modpack backend adds under modpackSourceConfig's root.
//
// It is empty for the local backend because modpack.LocalProvider writes at
// filepath.Join(base, key): the configured path IS the root. See
// storageSourceLocation for what appending one anyway did.
//
// It is deliberately NOT empty for the s3 backend, even though
// modpack.S3Provider likewise puts keys at the bucket root. That skew runs the
// other way: it reports the source key space as "modpacks" when it is really
// "", which can only ever make a genuinely distinct target compare EQUAL and be
// refused. Over-refusal is visible and workaroundable; the under-refusal a
// "correction" here would introduce is the destructive direction, and a target
// key space always carries the "modpacks" prefix, so it can never actually
// collide with the bucket root anyway.
func (r *StorageDataSetResolver) modpackSourceSubPrefix() string {
	provider, _ := r.state.Store.GetSetting("modpack_storage_provider")
	switch provider {
	case "s3", "core-storage":
		return CoreStoragePrefixModpacks
	default:
		return ""
	}
}

// modpackSourceConfig renders the modpack_storage_* settings in
// CoreStorageConfig shape, purely so sourceCoreStorageConfigFor can compare
// against them. Never persisted, never validated as a Core file storage config.
func (r *StorageDataSetResolver) modpackSourceConfig() CoreStorageConfig {
	get := func(k string) string {
		v, _ := r.state.Store.GetSetting(k)
		return v
	}
	switch get("modpack_storage_provider") {
	case "s3":
		return CoreStorageConfig{
			Backend:    "s3",
			S3Endpoint: get("modpack_storage_s3_endpoint"),
			S3Bucket:   get("modpack_storage_s3_bucket"),
		}
	case "core-storage":
		return r.state.LoadCoreStorageConfig()
	default:
		// modpack_storage_paths may list several mirrored roots; the first is
		// the one a same-location check can meaningfully compare against.
		paths := r.modpackConfiguredPaths()
		path := ""
		if len(paths) > 0 {
			path = strings.TrimSpace(paths[0])
		}
		return CoreStorageConfig{Backend: "path", Path: path, PathConfirmed: true}
	}
}

// coreStorageDataSetLocation is where ONE data set's objects physically live
// today, expressed in Core-file-storage terms so sameCoreStorageLocation can
// compare two of them.
//
// Unlike storageSourceLocation it carries a single sub-prefix, because both
// sides of a row-to-row comparison are "where this data set lives NOW". The
// source/target prefix split only exists for the ad-hoc-config path, where the
// target is a Core-storage-SHAPED destination that may nest differently from
// the source's own backend.
type coreStorageDataSetLocation struct {
	cfg       CoreStorageConfig
	subPrefix string
}

// dataSetLocation maps ANY data-set id to the physical location its objects
// occupy today, or reports that the id is not location-determinable.
//
// It is a deliberate SIBLING of sourceCoreStorageConfigFor rather than a reuse
// of it, because the two answer different questions. That one answers "may this
// id own a config switch?", so it REFUSES library, ticket-attachments,
// ticket-backups and modpacks@core-storage - which are exactly the ids this one
// must be able to locate. Conflating them is what left the row-to-row path with
// no same-location refusal at all.
//
// ok == false means "this id's location cannot be determined from here", not
// "this id is distinct from everything". Callers must treat unknown as
// permissive: a server-backups row's location lives in its provider-specific
// Config, and refusing every pair we cannot locate would kill
// server-backups:a -> server-backups:b, a documented legitimate flow.
func (r *StorageDataSetResolver) dataSetLocation(id string) (coreStorageDataSetLocation, bool) {
	switch id {
	case CoreStorageDataSetID:
		return coreStorageDataSetLocation{cfg: r.state.LoadCoreStorageConfig()}, true
	case storagemigrate.DataSetLibrary:
		return coreStorageDataSetLocation{cfg: r.state.LoadCoreStorageConfig(), subPrefix: CoreStoragePrefixLibrary}, true
	case storagemigrate.DataSetAttachments:
		return coreStorageDataSetLocation{cfg: r.state.LoadCoreStorageConfig(), subPrefix: CoreStoragePrefixAttachments}, true
	case storagemigrate.DataSetTicketBackups:
		return coreStorageDataSetLocation{cfg: r.state.LoadCoreStorageConfig(), subPrefix: CoreStoragePrefixBackups}, true
	case ModpacksCoreStorageDataSetID:
		return coreStorageDataSetLocation{cfg: r.state.LoadCoreStorageConfig(), subPrefix: CoreStoragePrefixModpacks}, true
	case storagemigrate.DataSetModpacks:
		// The modpacks data set follows its OWN settings namespace, which may
		// point at a local path, an s3 bucket, or back at Core storage - the
		// last of which makes it the very same location as
		// modpacks@core-storage.
		return coreStorageDataSetLocation{cfg: r.modpackSourceConfig(), subPrefix: r.modpackSourceSubPrefix()}, true
	}

	// A backup_storages row with provider "core-storage" ALWAYS lands under
	// backup.CoreStorageSubPrefix in the shared Core file storage, whatever its
	// Config says - Open ignores the Config entirely for that provider. Two such
	// rows are therefore one location, and this workstream allowlisted
	// "core-storage" as a backup provider, so creating a second one is trivial.
	// backupStorageLabel is only "Name (provider)", so their labels differ by
	// design and no label-based mitigation can catch this.
	//
	// Every other provider keeps its location in the row's Config, which this
	// function does not parse; those stay not-determinable.
	if storageID, ok := storagemigrate.ParseServerBackupsDataSetID(id); ok {
		bs, err := r.state.Store.GetBackupStorage(storageID)
		if err == nil && bs != nil && bs.Provider == "core-storage" {
			return coreStorageDataSetLocation{
				cfg:       r.state.LoadCoreStorageConfig(),
				subPrefix: CoreStoragePrefixServerBackups,
			}, true
		}
	}
	return coreStorageDataSetLocation{}, false
}

// EnsureDistinctDataSetLocations refuses a source/target data-set pair that
// resolves to one physical location. See the interface doc in
// services.StorageDataSetResolver for why the check has to exist at all.
//
// It compares LOCATIONS through sameCoreStorageLocation - the same comparator
// the ad-hoc-config path uses, which is location-complete (filepath.Clean fast
// path plus os.SameFile, so a symlink, junction or bind-mount alias is caught;
// trimmed endpoint + bucket + effective prefix for s3) - and never labels.
//
// It refuses two shapes of overlap, with separate messages because they fail
// differently:
//
//	EQUALITY - the pair is one location under two names. The copy rewrites every
//	object onto itself and the verification compares that location against its
//	own manifest.
//
//	CONTAINMENT - one side nests inside the other on the same root, e.g.
//	library -> core-storage (library/ lives inside the storage root) or
//	core-storage -> modpacks@core-storage. Data-set keys are relative to their
//	own sub-prefix, so copying across a nesting boundary rebases them into the
//	OTHER data set's live namespace, and copyOne overwrites on a checksum
//	mismatch. MkdirLibraryHandler lets an operator create a library folder
//	called "modpacks", which is all it takes to overwrite live modpack objects.
//	This is not over-refusal: a nested pair is never a meaningful migration,
//	because the contained side is already stored inside the containing one.
//
// What it deliberately does NOT refuse: a pair where either side is not
// location-determinable. Inequality is not proof, and refusing on unknown would
// kill server-backups:a -> server-backups:b.
func (r *StorageDataSetResolver) EnsureDistinctDataSetLocations(_ context.Context, sourceID, targetID string) error {
	src, srcOK := r.dataSetLocation(sourceID)
	tgt, tgtOK := r.dataSetLocation(targetID)
	if !srcOK || !tgtOK {
		return nil
	}
	same, err := sameCoreStorageLocation(src.cfg, tgt.cfg, src.subPrefix, tgt.subPrefix)
	if err != nil {
		return err
	}
	if same {
		return fmt.Errorf("%w: data sets %q and %q are different names for one physical location, so copying between them would rewrite every object onto itself and the verification would compare that location against its own manifest; pick a target that lives somewhere else",
			ErrTargetSameLocation, sourceID, targetID)
	}

	// Containment is only possible on a shared ROOT, so the sub-prefixes are
	// compared only once the roots are known to match. Passing "" here asks
	// sameCoreStorageLocation the root-level question with the same
	// symlink/bind-mount-aware comparator used above.
	sameRoot, err := sameCoreStorageLocation(src.cfg, tgt.cfg, "", "")
	if err != nil {
		return err
	}
	if !sameRoot {
		return nil
	}
	if subPrefixContains(tgt.subPrefix, src.subPrefix) {
		return fmt.Errorf("%w: data set %q is stored INSIDE %q, so copying it there would rebase its keys into the other data set's live namespace and overwrite any object whose name collides; there is nothing to move, the objects are already there",
			ErrTargetNestedLocation, sourceID, targetID)
	}
	if subPrefixContains(src.subPrefix, tgt.subPrefix) {
		return fmt.Errorf("%w: data set %q is stored INSIDE %q, so copying the outer one into it would rebase every key into the inner data set's live namespace and overwrite any object whose name collides",
			ErrTargetNestedLocation, targetID, sourceID)
	}
	return nil
}

// subPrefixContains reports whether outer strictly nests inner: every object
// stored under inner also sits inside outer. The empty sub-prefix is the
// storage ROOT and therefore contains every other data set.
//
// Equality returns false on purpose. That is the same-location case, which the
// caller has already refused with a message describing that failure instead.
// The comparison is segment-aware so a future sub-prefix "modpacks-archive" is
// not read as living inside "modpacks".
func subPrefixContains(outer, inner string) bool {
	outer = strings.Trim(outer, "/")
	inner = strings.Trim(inner, "/")
	if outer == inner {
		return false
	}
	if outer == "" {
		return true
	}
	return strings.HasPrefix(inner, outer+"/")
}

// ResolveTarget builds the target DataSet from an AD-HOC storage config, i.e.
// one supplied in the start request and never saved. This is what makes the
// Core-file-storage data sets migratable at all: their saved config says where
// they live now, so the destination has to be named separately.
//
// Order matters. Validate, then refuse a same-location target, and only then
// build a provider. The refusal has to land HERE, at Start, because by the time
// the copy loop noticed it would already have rewritten source objects onto
// themselves.
// targetCoreStorageConfig resolves a migration target into a CoreStorageConfig,
// loading a referenced storage connection when one is set. For a connection
// target the credentials come from the connection (decrypted in the store), the
// backend is s3, and ConnectionID is preserved so a switch persists the
// reference. For an inline target it is the plain field mapping.
func (r *StorageDataSetResolver) targetCoreStorageConfig(tc services.StorageTargetConfig) (CoreStorageConfig, error) {
	if tc.ConnectionID != 0 {
		conn, err := r.state.Store.GetStorageConnection(tc.ConnectionID)
		if err != nil {
			return CoreStorageConfig{}, fmt.Errorf("target storage connection %d: %w", tc.ConnectionID, err)
		}
		cfg := coreStorageConfigFromConnection(conn)
		cfg.ConnectionID = tc.ConnectionID
		return cfg, nil
	}
	return coreStorageConfigFromTarget(tc), nil
}

func (r *StorageDataSetResolver) ResolveTarget(_ context.Context, sourceID string, tc services.StorageTargetConfig) (storagemigrate.DataSet, string, error) {
	loc, err := r.sourceCoreStorageConfigFor(sourceID)
	if err != nil {
		return nil, "", err
	}
	tgtCfg, err := r.targetCoreStorageConfig(tc)
	if err != nil {
		return nil, "", err
	}
	if err := validateCoreStorageConfig(tgtCfg); err != nil {
		return nil, "", fmt.Errorf("target storage config: %w", err)
	}
	if err := ensureDistinctCoreStorageLocation(loc.cfg, tgtCfg, loc.srcSubPrefix, loc.tgtSubPrefix); err != nil {
		return nil, "", err
	}
	prov, err := buildTargetStorageProvider(tgtCfg, loc.tgtSubPrefix)
	if err != nil {
		return nil, "", err
	}
	// The label is credential-free by construction; it is the ONLY thing about
	// this config that reaches the job record, the logs or the audit.
	label := coreStorageBackendLabel(tgtCfg, loc.tgtSubPrefix)

	if sourceID == storagemigrate.DataSetModpacks {
		return storagemigrate.NewModpackDataSet(sourceID, "Migration target", modpack.NewCoreStorageProvider(prov), r.state.Store), label, nil
	}
	return storagemigrate.NewProviderDataSet(sourceID, "Migration target", prov), label, nil
}

// SwitchConfig makes the target config the ACTIVE one for sourceID. Called only
// from the job's switching_config phase, only after a passing verification, and
// it is the one and only place the target's S3 secret is persisted.
//
// It writes through persistCoreStorageConfig, the same writer SaveConfig uses,
// so the settings form and the migration cannot drift apart.
func (r *StorageDataSetResolver) SwitchConfig(ctx context.Context, sourceID string, tc services.StorageTargetConfig) error {
	if _, err := r.sourceCoreStorageConfigFor(sourceID); err != nil {
		return err
	}
	cfg, err := r.targetCoreStorageConfig(tc)
	if err != nil {
		return err
	}
	if err := validateCoreStorageConfig(cfg); err != nil {
		return fmt.Errorf("target storage config: %w", err)
	}
	if sourceID == storagemigrate.DataSetModpacks {
		// Modpacks have their own settings namespace; point THOSE at the
		// target rather than repointing the shared Core file storage, which
		// three other data sets also read. The host-path guard below is NOT
		// applied here on purpose: the modpack settings form has no such guard
		// either, so guarding only the migration path would be an asymmetry in
		// the other direction. Modpacks on a multi-Core host path stay the
		// modpack settings form's concern, not this one's.
		return r.switchModpackConfig(cfg)
	}
	// The same shared-storage proof the settings form requires through
	// checkSharedStorageReachable. Without it a migration to a target no other
	// Core can actually reach persists with no check at all, landing the
	// deployment in exactly the split-storage state the guard on SaveConfig
	// exists to prevent - and, unlike the count-only guard this replaced, it
	// also catches an s3 target with credentials that do not work. The job
	// surfaces this error with the source left intact and nothing deleted.
	if err := r.state.checkSharedStorageReachable(ctx, cfg); err != nil {
		return err
	}
	// cfg.S3SecretKey, not a "was a secret submitted" flag: persistCoreStorageConfig
	// takes the secret VALUE so the only thing it can ever write is the one
	// handed to it (see its doc comment - a boolean form let a secret-only
	// rotation silently no-op in an earlier review).
	return r.state.persistCoreStorageConfig(cfg, cfg.S3SecretKey)
}

// switchModpackConfig repoints the modpack_storage_* settings at cfg.
func (r *StorageDataSetResolver) switchModpackConfig(cfg CoreStorageConfig) error {
	set := func(k, v string) error {
		if err := r.state.Store.SetSetting(k, v); err != nil {
			return fmt.Errorf("save setting %s: %w", k, err)
		}
		return nil
	}
	// Point (or un-point) modpack storage at a saved connection. Written
	// unconditionally so an inline switch clears a previously-selected connection
	// too; buildModpackStorageProvider checks this id before the inline settings.
	if err := set(keyModpackStorageConnectionID, storageConnIDSetting(cfg.ConnectionID)); err != nil {
		return err
	}
	if cfg.Backend == "s3" {
		for _, p := range []struct{ k, v string }{
			{"modpack_storage_provider", "s3"},
			{"modpack_storage_s3_endpoint", cfg.S3Endpoint},
			{"modpack_storage_s3_bucket", cfg.S3Bucket},
			{"modpack_storage_s3_region", cfg.S3Region},
			{"modpack_storage_s3_access_key", cfg.S3AccessKey},
			{"modpack_storage_s3_secret_key", cfg.S3SecretKey},
		} {
			if err := set(p.k, p.v); err != nil {
				return err
			}
		}
		return nil
	}
	if err := set("modpack_storage_provider", "local"); err != nil {
		return err
	}
	// Clear the stored s3 secret when the backend leaves s3, mirroring
	// persistCoreStorageConfig: an orphaned credential for a backend nothing
	// reads any more is a liability, and it leaves the settings form reporting
	// a secret is set for a provider that has none.
	if err := set("modpack_storage_s3_secret_key", ""); err != nil {
		return err
	}
	pathsJSON, _ := json.Marshal([]string{coreStorageRoot(cfg, CoreStoragePrefixModpacks)})
	return set("modpack_storage_paths", string(pathsJSON))
}

// storageMigrationAuditPayload builds the audit detail for a start request.
// The target config is reduced to its credential-free label: an audit row is
// long-lived and widely readable, so a secret landing there would outlive and
// out-share the request that carried it.
func storageMigrationAuditPayload(req services.StorageMigrationRequest, jobID string) map[string]interface{} {
	target := req.TargetDataSet
	if req.TargetConfig != nil {
		if req.TargetConfig.ConnectionID != 0 {
			// A connection target has no inline credentials to label; name the
			// reference instead. Still credential-free.
			target = fmt.Sprintf("storage-connection:%d", req.TargetConfig.ConnectionID)
		} else {
			target = coreStorageBackendLabel(coreStorageConfigFromTarget(*req.TargetConfig), req.DataSet)
		}
	}
	return map[string]interface{}{
		"action":       "storage_migration_start",
		"kind":         string(req.Kind),
		"dataSet":      req.DataSet,
		"target":       target,
		"verifyMode":   req.VerifyMode,
		"deleteSource": req.DeleteSource,
		"jobId":        jobID,
	}
}

// backupDeps mirrors BackupHandler.backupDeps so the resolver can open a
// core-storage backup row too.
func (r *StorageDataSetResolver) backupDeps() backupstorage.Deps {
	return backupstorage.Deps{
		Registry:  r.state.GRPCRegistry,
		NodeStore: r.state.Store,
		CoreStorage: func(subPrefix string) (backupstorage.Storage, error) {
			prov, err := r.state.buildCoreStorageProvider(subPrefix)
			if err != nil {
				return nil, err
			}
			return storage.NewCoreStorageBackupAdapter(prov), nil
		},
		Connection: r.state.ConnectionBackupBuilder(),
	}
}

// --- HTTP surface ---

// StorageMigrationHandler exposes the in-panel blob storage migration. All
// endpoints are gated at the route by AuthMiddleware + RequireCap
// (settings.read for reads, settings.write for mutations), matching the
// db-migration and core-storage precedent exactly.
type StorageMigrationHandler struct {
	state *AppState
}

func NewStorageMigrationHandler(state *AppState) *StorageMigrationHandler {
	return &StorageMigrationHandler{state: state}
}

// Overview GET /api/admin/storage/overview - PANEL settings.read.
func (h *StorageMigrationHandler) Overview(w http.ResponseWriter, r *http.Request) {
	sets, err := NewStorageDataSetResolver(h.state).List(r.Context())
	if err != nil {
		sendJSONError(w, "Failed to describe storage: "+err.Error(), http.StatusInternalServerError)
		return
	}
	type overviewEntry struct {
		services.StorageDataSetInfo
		LatestManifest *models.StorageManifest `json:"latestManifest"`
	}
	out := make([]overviewEntry, 0, len(sets))
	for _, s := range sets {
		e := overviewEntry{StorageDataSetInfo: s}
		// Surfaced, not swallowed: rendering a storage-layer failure as "no
		// manifest yet" would tell an operator their last capture is gone.
		ms, err := h.state.Store.ListStorageManifests(s.ID, 1)
		if err != nil {
			sendJSONError(w, "Failed to load manifests for "+s.ID+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		if len(ms) > 0 {
			e.LatestManifest = &ms[0]
		}
		out = append(out, e)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "dataSets": out})
}

// GetJob GET /api/admin/storage/migration - PANEL settings.read.
func (h *StorageMigrationHandler) GetJob(w http.ResponseWriter, r *http.Request) {
	if h.state.StorageMigration == nil {
		sendJSONError(w, "Storage migration not available", http.StatusServiceUnavailable)
		return
	}
	job, ok, err := h.state.StorageMigration.GetJob(r.Context())
	if err != nil {
		sendJSONError(w, "Failed to read job: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]interface{}{"success": true, "hasJob": ok}
	if ok {
		resp["job"] = job
	}
	json.NewEncoder(w).Encode(resp)
}

// Start POST /api/admin/storage/migration - PANEL settings.write. Returns 202
// with the initial job; 409 when one is already running; 400 on a bad request.
func (h *StorageMigrationHandler) Start(w http.ResponseWriter, r *http.Request) {
	var req services.StorageMigrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	// Validate BEFORE the service-availability check, so a request that
	// violates safety invariant 2 is rejected as 400 regardless of whether
	// Redis happens to be up.
	if err := req.Validate(); err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.state.StorageMigration == nil {
		sendJSONError(w, "Storage migration not available", http.StatusServiceUnavailable)
		return
	}

	actorID, _ := r.Context().Value("userID").(string)
	actorName := h.resolveUsername(actorID)

	job, err := h.state.StorageMigration.Start(r.Context(), req, actorID, actorName)
	if err != nil {
		if errors.Is(err, services.ErrStorageMigrationRunning) {
			sendJSONError(w, err.Error(), http.StatusConflict)
			return
		}
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	LogIdentityAudit(h.state, r, AuditEventStorageMigrationStart, actorID, "", storageMigrationAuditPayload(req, job.ID))
	if h.state.Events != nil {
		h.state.Events.Publish(r.Context(), "storagemigration.changed", nil)
	}

	// The response carries the JOB, never the request: StorageMigrationJob has
	// no target-config field at all (Task 13), so the target's S3 secret cannot
	// be echoed back to the caller.
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "job": job})
}

// Cancel POST /api/admin/storage/migration/cancel - PANEL settings.write.
func (h *StorageMigrationHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	if h.state.StorageMigration == nil {
		sendJSONError(w, "Storage migration not available", http.StatusServiceUnavailable)
		return
	}
	if err := h.state.StorageMigration.Cancel(r.Context()); err != nil {
		if errors.Is(err, services.ErrNoStorageMigrationJob) {
			sendJSONError(w, err.Error(), http.StatusNotFound)
			return
		}
		sendJSONError(w, err.Error(), http.StatusConflict)
		return
	}
	actorID, _ := r.Context().Value("userID").(string)
	LogIdentityAudit(h.state, r, AuditEventStorageMigrationCancel, actorID, "", map[string]interface{}{
		"action": "storage_migration_cancel",
	})
	if h.state.Events != nil {
		h.state.Events.Publish(r.Context(), "storagemigration.changed", nil)
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ListManifests GET /api/admin/storage/manifests - PANEL settings.read.
// Optional ?dataSet= filter and ?limit=.
func (h *StorageMigrationHandler) ListManifests(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ms, err := h.state.Store.ListStorageManifests(r.URL.Query().Get("dataSet"), limit)
	if err != nil {
		sendJSONError(w, "Failed to list manifests: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if ms == nil {
		ms = []models.StorageManifest{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "manifests": ms})
}

// ExportManifest GET /api/admin/storage/manifests/{id}/export - PANEL
// settings.read. Streams CSV (key,size,sha256), not JSON: a 100k-entry
// manifest as one JSON blob is hostile to both the browser and whatever
// out-of-band tooling the admin uses to move the data. encoding/csv handles
// quoting, so a key containing a comma or a quote survives the round trip.
func (h *StorageMigrationHandler) ExportManifest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	m, err := h.state.Store.GetStorageManifest(id)
	if err != nil {
		sendJSONError(w, "Failed to load manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if m == nil {
		sendJSONError(w, "Manifest not found", http.StatusNotFound)
		return
	}
	entries, err := h.state.Store.ListStorageManifestEntries(id)
	if err != nil {
		sendJSONError(w, "Failed to load manifest entries: "+err.Error(), http.StatusInternalServerError)
		return
	}

	filename := fmt.Sprintf("dylaris-manifest-%d-%s.csv", m.ID, safeFilenamePart(m.DataSet))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")

	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{"key", "size", ChecksumColumnName(m.Algo)})
	for _, e := range entries {
		_ = cw.Write([]string{e.Key, strconv.FormatInt(e.Size, 10), e.Checksum})
	}
}

// ChecksumColumnName names the CSV checksum column after the manifest's algo,
// so an export is self-describing.
func ChecksumColumnName(algo string) string {
	if algo == "" {
		return storagemigrate.ChecksumAlgo
	}
	return algo
}

// safeFilenamePart strips anything that would need quoting in a
// Content-Disposition filename.
func safeFilenamePart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// DeleteManifest DELETE /api/admin/storage/manifests/{id} - PANEL
// settings.write. Entries cascade.
//
// An unknown id is a 404 and writes no audit row, matching ExportManifest on
// the same id. Reporting success for a manifest that was never there tells the
// operator a deletion happened, and an audit trail that records deletions which
// did not occur is worse than no entry at all.
func (h *StorageMigrationHandler) DeleteManifest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	m, err := h.state.Store.GetStorageManifest(id)
	if err != nil {
		sendJSONError(w, "Failed to load manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if m == nil {
		sendJSONError(w, "Manifest not found", http.StatusNotFound)
		return
	}
	if err := h.state.Store.DeleteStorageManifest(id); err != nil {
		sendJSONError(w, "Failed to delete manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}
	actorID, _ := r.Context().Value("userID").(string)
	LogIdentityAudit(h.state, r, AuditEventStorageManifestDeleted, actorID, "", map[string]interface{}{
		"action":     "storage_manifest_delete",
		"manifestId": id,
	})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// resolveUsername best-effort maps a user UUID to a display name.
func (h *StorageMigrationHandler) resolveUsername(id string) string {
	if id == "" {
		return ""
	}
	if u, err := h.state.Store.GetUserByID(id); err == nil && u != nil {
		return u.Username
	}
	return ""
}
