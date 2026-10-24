package handlers

import (
	"context"
	pb "dylaris-proto/node"
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

// --- gateway -----------------------------------------------------------------

func (s *AppState) GatewayEnabled() bool { return s.gatewayEnabled() }

func GatewayDisabledResponse(w http.ResponseWriter) { gatewayDisabledResponse(w) }

func (s *AppState) RequireEntitlement(r *http.Request, w http.ResponseWriter, userID, kind string) bool {
	return s.requireEntitlement(r, w, userID, kind)
}

func (s *AppState) UserOwnedByOther(ownerID *string, userID string) bool {
	return s.userOwnedByOther(ownerID, userID)
}

func (s *AppState) KitOwnedByOther(ownerID string, userID string) bool {
	return s.kitOwnedByOther(ownerID, userID)
}

func (s *AppState) DeployConfig() DeployConfig { return s.deployConfig() }

func MustAtoi(s string) int { return mustAtoi(s) }

func NormHost(s string) string { return normHost(s) }

func IsHostname(s string, minLabels int) bool { return isHostname(s, minLabels) }

func ValidateSubdomain(sub, mode string) bool { return validateSubdomain(sub, mode) }

func ByonActive(state *AppState, r *http.Request) bool { return byonActive(state, r) }

func RouteOnlyActive(state *AppState, r *http.Request) bool { return routeOnlyActive(state, r) }

func OwnerRefusal(state *AppState, r *http.Request, ownerID string) string {
	return ownerRefusal(state, r, ownerID)
}

func GenerateLinkIdentity() (string, error) { return generateLinkIdentity() }

func GenerateNodeWarpIdentity() (string, error) { return generateNodeWarpIdentity() }

func OwnNodeKey(st store.Store, nodeID, userID string) (*store.WarpAPIKey, int, string) {
	return ownNodeKey(st, nodeID, userID)
}

func NodeReleaseAtLeast(ctx context.Context, st *AppState, nodeToken, since string) bool {
	return nodeReleaseAtLeast(ctx, st, nodeToken, since)
}

func LinkAnnouncesCap(ctx context.Context, rdb *redis.Client, token, cap string) (bool, error) {
	return linkAnnouncesCap(ctx, rdb, token, cap)
}

func GrpcPortFromEnv() string { return grpcPortFromEnv() }

func OverlayServiceAddrs(grpcPort string) (coreAddr, redisAddr string) {
	return overlayServiceAddrs(grpcPort)
}

func SvcVoiceEnable(existing, host string, publicPort int) string {
	return svcVoiceEnable(existing, host, publicPort)
}

func SvcVoiceDisable(existing string) string { return svcVoiceDisable(existing) }

func PlasmoVoiceEnable(existing, host string, publicPort int) string {
	return plasmoVoiceEnable(existing, host, publicPort)
}

func PlasmoVoiceDisable(existing string) string { return plasmoVoiceDisable(existing) }

func (h *RconHandler) FindVoiceConfig(nodeID int, serverUUID, subServer string) (mod, relPath, content string, err error) {
	return h.findVoiceConfig(nodeID, serverUUID, subServer)
}

func (h *RconHandler) WriteNodeFileString(nodeID int, serverUUID, relPath, content string) error {
	return h.writeNodeFileString(nodeID, serverUUID, relPath, content)
}

// The voice-mod errors are compared with ==, so these are the same values.
var (
	ErrVoiceModMissing   = errVoiceModMissing
	ErrVoiceModAmbiguous = errVoiceModAmbiguous
)

const LinkCapClientAddr = linkCapClientAddr

var (
	DefaultMaxCustomDomains     = defaultMaxCustomDomains
	DefaultBlockedRoutePrefixes = defaultBlockedRoutePrefixes
)

func AdminRouteScope(userID string) string { return adminRouteScope(userID) }

const DefaultWarpSpokeAllowedPorts = defaultWarpSpokeAllowedPorts

// --- nodes -------------------------------------------------------------------

func ForeignToCaller(state *AppState, r *http.Request, nodeID int) bool {
	return foreignToCaller(state, r, nodeID)
}

func CanManageNode(state *AppState, r *http.Request, node *models.Node) bool {
	return canManageNode(state, r, node)
}

func OwnedByOther(state *AppState, r *http.Request, node *models.Node) bool {
	return ownedByOther(state, r, node)
}

func OwnershipInForce(state *AppState, r *http.Request) bool { return ownershipInForce(state, r) }

func PurgeBackupArchivesForServers(state *AppState, serverIDs []int) {
	purgeBackupArchivesForServers(state, serverIDs)
}

func DropDemoServers(s *AppState, uuids []string) { dropDemoServers(s, uuids) }

func AnnotateStalledInstallsFor(ctx context.Context, state *AppState, servers []models.Server) {
	annotateStalledInstallsFor(ctx, state, servers)
}

func ReadErrStatus(err error) int { return readErrStatus(err) }

func CollectNodeFile(ch <-chan *pb.NodeMessage, max int) ([]byte, *pb.OpError, error) {
	return collectNodeFile(ch, max)
}

func NodeErrorStatus(code int32) int { return nodeErrorStatus(code) }

const MaxOpenFileBytes = maxOpenFileBytes

func SetNodesLinkOnline(ctx context.Context, state *AppState, nodes []models.Node) {
	setNodesLinkOnline(ctx, state, nodes)
}

func InspectOrphanOnNode(state *AppState, nodeID int, orphanUUID string) (*pb.InspectOrphanResp, error) {
	return inspectOrphanOnNode(state, nodeID, orphanUUID)
}

func SubServerLoaderFromInspect(resp *pb.InspectOrphanResp, subName string) (instType, mcVer, build string, ok bool) {
	return subServerLoaderFromInspect(resp, subName)
}

func IsBYONNode(n models.Node) bool { return isBYONNode(n) }

type DylarisMetadata = dylarisMetadata

// --- mods --------------------------------------------------------------------

func SendCooldown(w http.ResponseWriter, upstream string, left time.Duration) {
	sendCooldown(w, upstream, left)
}

// UpstreamCooldown is the per-upstream back-off a proxy handler embeds.
type UpstreamCooldown = upstreamCooldown

func (c *upstreamCooldown) Blocked(now time.Time) (time.Duration, bool) { return c.blocked(now) }

func (c *upstreamCooldown) Trip(h http.Header, now time.Time) time.Duration { return c.trip(h, now) }

func HashURL(s string) string { return hashURL(s) }

const MaxCachedResponseBytes = maxCachedResponseBytes

const KeyModpackStorageConnectionID = keyModpackStorageConnectionID

func StorageConnIDSetting(id int) string { return storageConnIDSetting(id) }

func BoolStr(b bool) string { return boolStr(b) }

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

func MayManageAccount(state *AppState, r *http.Request, target *models.User) bool {
	return mayManageAccount(state, r, target)
}

func ActingOnSelf(r *http.Request, targetID string) bool { return actingOnSelf(r, targetID) }

// AdminReauthRequest is embedded in the admin account-action bodies; the alias
// keeps the JSON shape ({"reauth": {...}}) exactly as it was.
type (
	AdminReauthRequest = adminReauthRequest
	ReauthFields       = reauthFields
)

func RequireAdminReauth(w http.ResponseWriter, r *http.Request, state *AppState, body adminReauthRequest) bool {
	return requireAdminReauth(w, r, state, body)
}

func RequireReauth(state *AppState, userID, password, code string) *reauthError {
	return requireReauth(state, userID, password, code)
}

func WriteReauthError(w http.ResponseWriter, e *reauthError) { writeReauthError(w, e) }

func NotifyPasswordChanged(state *AppState, email, username string) {
	notifyPasswordChanged(state, email, username)
}

func RevokeAllAPIKeys(state *AppState, userID, why string) int {
	return revokeAllAPIKeys(state, userID, why)
}

func RevokeAllSSHKeys(state *AppState, userID, why string) int {
	return revokeAllSSHKeys(state, userID, why)
}

func StampBeamAccessForUser(ctx context.Context, state *AppState, userID string) {
	stampBeamAccessForUser(ctx, state, userID)
}

func StampBeamUUIDs(ctx context.Context, state *AppState, uuids []string) {
	stampBeamUUIDs(ctx, state, uuids)
}

func BeamReachOf(state *AppState, userID string) []string { return beamReachOf(state, userID) }

func CatalogCaps(caps []string) []string { return catalogCaps(caps) }

func ValidatePanelCaps(caps []string) error { return validatePanelCaps(caps) }

// SetAccountMailSender replaces the account notice sender for a test and
// returns the restore func. Production never calls it.
func SetAccountMailSender(fn func(services.MailStore, string, string, string, map[string]string) error) (restore func()) {
	prev := sendAccountMail
	sendAccountMail = fn
	return func() { sendAccountMail = prev }
}

func ApplyPlacementScope(state *AppState, r *http.Request, req *PickNodeRequest) {
	applyPlacementScope(state, r, req)
}

func ApplyResolvedTabPermissions(state *AppState, servers []models.Server, userID, username string) []models.Server {
	return applyResolvedTabPermissions(state, servers, userID, username)
}

var BungeeCordBuildPattern = bungeeCordBuildPattern

func CanPlaceOnNode(state *AppState, r *http.Request, node *models.Node) bool {
	return canPlaceOnNode(state, r, node)
}

func CountSubServerDirs(entries []*pb.FileInfo) int { return countSubServerDirs(entries) }

func DiskMBToGBCeil(mb int64) int { return diskMBToGBCeil(mb) }

func EditDemoList(st store.Store, edit func([]string) ([]string, error)) error {
	return editDemoList(st, edit)
}

var EffectiveCpuset = effectiveCpuset

var ErrDemoServerGone = errDemoServerGone

func ExactMinecraftVersion(software, mcVersion, version, loader string) string {
	return exactMinecraftVersion(software, mcVersion, version, loader)
}

func InstallStatusFor(ctx context.Context, st *AppState, nodeToken string) string {
	return installStatusFor(ctx, st, nodeToken)
}

func ListServerDirFor(state *AppState, nodeID int, serverUUID, dir string) ([]*pb.FileInfo, error) {
	return listServerDirFor(state, nodeID, serverUUID, dir)
}

func LoadDemoServerUUIDs(st store.Store) []string { return loadDemoServerUUIDs(st) }

func ModFitKnown(serverLoader, serverMC string) bool { return modFitKnown(serverLoader, serverMC) }

func ModVersionFits(serverLoader, serverMC string, versionLoaders, versionGameVersions []string) (bool, string) {
	return modVersionFits(serverLoader, serverMC, versionLoaders, versionGameVersions)
}

func NeoForgeMinecraft(loader string) string { return neoForgeMinecraft(loader) }

func NodeBusyKey(uuid string) string { return nodeBusyKey(uuid) }

func NodeReleaseOlderThan(ctx context.Context, st *AppState, nodeToken, since string) bool {
	return nodeReleaseOlderThan(ctx, st, nodeToken, since)
}

func NormalizeCaps(caps []string) []string { return normalizeCaps(caps) }

func OperatorOverride(r *http.Request) bool { return operatorOverride(r) }

var PaperMCReleasePattern = paperMCReleasePattern

func ProxyBackendUUIDs(state *AppState, proxyID int) []string {
	return proxyBackendUUIDs(state, proxyID)
}

func ProxyInstallRefusal(ctx context.Context, st *AppState, serverType, nodeToken, installerType string) (int, string) {
	return proxyInstallRefusal(ctx, st, serverType, nodeToken, installerType)
}

var ProxyInstallers = proxyInstallers

const ProxyProtocolDefaultSince = proxyProtocolDefaultSince

func ProxyProtocolInstallGate(w http.ResponseWriter, srv *models.Server, installerType, version string) (keep, refused bool) {
	return proxyProtocolInstallGate(w, srv, installerType, version)
}

var RamPaddingRefused = ramPaddingRefused

func RconNeedsStamping(subServer string, enabled bool, password string) bool {
	return rconNeedsStamping(subServer, enabled, password)
}

func RedactNodeAddress(state *AppState, servers []models.Server, isAdmin bool, userID string) {
	redactNodeAddress(state, servers, isAdmin, userID)
}

func RedactSettingsFields(s *models.Server) { redactSettingsFields(s) }

func RefuseIfMoving(w http.ResponseWriter, r *http.Request, srv *models.Server) bool {
	return refuseIfMoving(w, r, srv)
}

func RefuseIfOverSubServerLimit(w http.ResponseWriter, limit *int64, have int) bool {
	return refuseIfOverSubServerLimit(w, limit, have)
}

func RefuseIfSubServerLimitReached(w http.ResponseWriter, state *AppState, nodeID int, serverUUID, target string) bool {
	return refuseIfSubServerLimitReached(w, state, nodeID, serverUUID, target)
}

func ResolveLibraryInstall(ctx context.Context, state *AppState, rawPath string, isAdmin bool) (string, int, string) {
	return resolveLibraryInstall(ctx, state, rawPath, isAdmin)
}

func RevokeUncoversInheritance(state *AppState, srv *models.Server, targetID string) bool {
	return revokeUncoversInheritance(state, srv, targetID)
}

func SendTechnicError(w http.ResponseWriter, err error) { sendTechnicError(w, err) }

func StampBeamAccess(ctx context.Context, state *AppState, serverID int) {
	stampBeamAccess(ctx, state, serverID)
}

func StampBeamAccessForOwner(ctx context.Context, state *AppState, ownerUserID string) {
	stampBeamAccessForOwner(ctx, state, ownerUserID)
}

func StrictPanelCaps(state *AppState, userID string) (map[string]bool, bool) {
	return strictPanelCaps(state, userID)
}

func SubServerLimit(state *AppState) *int64 { return subServerLimit(state) }

const TechnicSince = technicSince

const UpstreamCooldownDefault = upstreamCooldownDefault

// PickNodeFor runs the placement decision for a server create; the HTTP
// PickNode handler is a different, request-shaped entry point.
func (h *PlacementHandler) PickNodeFor(ctx context.Context, req PickNodeRequest) PickNodeResponse {
	return h.pickNode(ctx, req)
}

func (h *TechnicHandler) ResolveTechnic(ctx context.Context, slug, build string) (*TechnicResolution, error) {
	return h.resolveTechnic(ctx, slug, build)
}

// ApplyProps writes the RCON keys into a sub-server's server.properties through
// the handler's applyProps seam.
func (h *RconHandler) ApplyProps(nodeID int, serverUUID, activeSubServer string, enabled bool, port int, password string) error {
	return h.applyProps(nodeID, serverUUID, activeSubServer, enabled, port, password)
}

type ApiKeyCtx = apiKeyCtx

type ApiKeyCtxKey = apiKeyCtxKey

var DefaultPlacementSettings = defaultPlacementSettings

const ModReportingSince = modReportingSince

func NeoForgeMajor(v string) (string, bool) { return neoForgeMajor(v) }

var SubRegexDNS = subRegexDNS

const LibraryMirrorTTL = libraryMirrorTTL

func LibraryMirrorURL(base, secret, libPath string, now time.Time) string {
	return libraryMirrorURL(base, secret, libPath, now)
}

func (h *PlacementHandler) ScoreNode(ctx context.Context, n *models.Node, req PickNodeRequest, hb *services.NodeHeartbeat) NodeCandidate {
	return h.scoreNode(ctx, n, req, hb)
}

// NewAPIKeyCtx is the request context an API key login leaves behind; tests
// outside this package build it to drive the external endpoints.
func NewAPIKeyCtx(k *models.APIKey) *ApiKeyCtx { return &apiKeyCtx{key: k} }

const DemoServerUUIDsSetting = demoServerUUIDsSetting
