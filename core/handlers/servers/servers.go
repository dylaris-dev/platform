package servers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"dylaris-core/handlers/access"
	"dylaris-core/handlers/gate"
	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/nodeops"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/demo"
)

// defaultJvmFlags are always injected into the start command but not stored in extra_jvm_flags.
const defaultJvmFlags = services.DefaultJvmFlags

// aikarsFlags are Aikar's optimized G1GC flags for Minecraft servers (standard RAM).
const aikarsFlags = "-XX:+UseG1GC -XX:+ParallelRefProcEnabled -XX:MaxGCPauseMillis=200 " +
	"-XX:+UnlockExperimentalVMOptions -XX:+DisableExplicitGC -XX:+AlwaysPreTouch " +
	"-XX:G1NewSizePercent=30 -XX:G1MaxNewSizePercent=40 -XX:G1HeapRegionSize=8M " +
	"-XX:G1ReservePercent=20 -XX:G1HeapWastePercent=5 -XX:G1MixedGCCountTarget=4 " +
	"-XX:InitiatingHeapOccupancyPercent=15 -XX:G1MixedGCLiveThresholdPercent=90 " +
	"-XX:G1RSetUpdatingPauseTimePercent=5 -XX:SurvivorRatio=32 " +
	"-XX:+PerfDisableSharedMem -XX:MaxTenuringThreshold=1"

// aikarsHighMemFlags are Aikar's flags tuned for 12GB+ RAM servers.
const aikarsHighMemFlags = "-XX:+UseG1GC -XX:+ParallelRefProcEnabled -XX:MaxGCPauseMillis=200 " +
	"-XX:+UnlockExperimentalVMOptions -XX:+DisableExplicitGC -XX:+AlwaysPreTouch " +
	"-XX:G1NewSizePercent=40 -XX:G1MaxNewSizePercent=50 -XX:G1HeapRegionSize=16M " +
	"-XX:G1ReservePercent=20 -XX:G1HeapWastePercent=5 -XX:G1MixedGCCountTarget=4 " +
	"-XX:InitiatingHeapOccupancyPercent=15 -XX:G1MixedGCLiveThresholdPercent=90 " +
	"-XX:G1RSetUpdatingPauseTimePercent=5 -XX:SurvivorRatio=32 " +
	"-XX:+PerfDisableSharedMem -XX:MaxTenuringThreshold=1"

// ==========================================
// REQUEST TYPES
// ==========================================

type CreateServerRequest struct {
	UUID       string   `json:"uuid"`
	Name       string   `json:"name"`
	NodeID     string   `json:"nodeId"`
	Region     string   `json:"region"` // optional scheduler filter (e.g. "eu-central")
	Tags       []string `json:"tags"`   // AND-filter when scheduler picks a node
	Tag        string   `json:"tag"`    // deprecated, single-tag legacy field; folded into Tags
	OwnerID    string   `json:"ownerId"`
	IsFixed    *bool    `json:"isFixed"`
	ServerType string   `json:"serverType"`
	AutoMove   bool     `json:"autoMove"` // opt-in to load-balancing migrations
	// StoragePath picks one of the node's storage paths; empty lets the node
	// choose. The node accepts only a path it has.
	StoragePath string `json:"storagePath"`
	Docker      struct {
		RAM       int     `json:"ram"`
		CPULimit  float64 `json:"cpuLimit"`
		DiskLimit int64   `json:"diskLimit"`
		// RAMPaddingMB overrides the container RAM padding; nil inherits.
		RAMPaddingMB *int `json:"ramPaddingMb"`
	} `json:"docker"`
}

type SetupServerRequest struct {
	SubServerName string `json:"subServerName"`
	JavaImage     string `json:"javaImage"`
	ExtraJvmFlags string `json:"extraJvmFlags"`
	// CreatesRoute: the panel adds a gateway route right after this setup
	// answers. Only then may a new server default to real player IP.
	CreatesRoute bool `json:"createsRoute,omitempty"`
	// AcceptMinecraftEula: the caller accepts the Minecraft EULA with this
	// install. Needed once per account; see requireMinecraftEula.
	AcceptMinecraftEula bool `json:"acceptMinecraftEula,omitempty"`
	Installer           struct {
		Type      string `json:"type"`      // "paper","vanilla","fabric","forge","neoforge","upload","upload-zip","modpack","pack"
		Version   string `json:"version"`   // build/version identifier (Paper build, Forge build, etc.)
		McVersion string `json:"mcVersion"` // major MC version (e.g. "1.21.4")
		Loader    string `json:"loader"`    // Fabric loader / Forge build / NeoForge version (optional)
		URL       string `json:"url"`       // for import via URL OR .mrpack url for modpack
		Structure string `json:"structure"` // "direct" or "subfolder" (for upload-zip)
		// Modpack reference; sub-server boots from a Modrinth
		// modpack and we remember which project+version so the panel can
		// later check Modrinth for newer versions and offer one-click update.
		ModrinthProjectID   string `json:"modrinthProjectId,omitempty"`
		ModrinthVersionID   string `json:"modrinthVersionId,omitempty"`
		ModrinthProjectSlug string `json:"modrinthProjectSlug,omitempty"`
		// Unified pack reference; installer.type=="pack" carries these instead
		// of a raw URL. SetupServer resolves+authorizes the pack/build, renders
		// its .mrpack, and rewrites Installer in place to type="modpack" before
		// dispatch — the Node never sees type="pack".
		PackID  int `json:"packId,omitempty"`
		BuildID int `json:"buildId,omitempty"`
		// Technic pack; installer.type=="technic". SetupServer resolves both
		// against Technic itself (resolveTechnic). TechnicBuild matters only for
		// Solder packs and defaults to the pack's recommended build.
		TechnicSlug  string `json:"technicSlug,omitempty"`
		TechnicBuild string `json:"technicBuild,omitempty"`
		// WipePaths names what to CLEAR before installing. Tokens, never paths -
		// see installWipeTokens. Empty means install on top of whatever is there,
		// which is what every install did before this existed and is still right
		// for a plain jar swap.
		WipePaths []string `json:"wipePaths,omitempty"`
		// Software applies to "upload" and "upload-zip" only: the server
		// software (one of reinstallableInstallers) the node installs over the
		// uploaded files, with Version/McVersion/Loader as for an online
		// install. Empty means "keep the jar in the upload".
		Software string `json:"software,omitempty"`
		// TemplateID: installer.type=="template". SetupServer checks the caller
		// may see it, pushes its files to the node and rewrites Installer to
		// upload-zip with the template's software (template_install.go).
		TemplateID int `json:"templateId,omitempty"`
	} `json:"installer"`
}

// installWipeTokens is the vocabulary a caller may use to ask for a directory to
// be cleared before an install.
//
// A FROZEN cross-repo contract with platform/node (installer_wipe.go), which
// keeps its own copy and refuses anything it does not know. Both sides validate
// on purpose: this list keeps a bad request out of the queue, the node's keeps a
// bad queue entry from deleting anything. Adding a token means adding it in both
// places, and the node is the one that must ship first.
//
// Tokens rather than paths because this is the one instruction that destroys a
// tenant's files. A path on the wire is a deletion primitive; a token can only
// ever name something that was designed in.
var installWipeTokens = map[string]bool{
	"mods":      true, // the pack's mods
	"config":    true, // config/ and defaultconfigs/
	"libraries": true, // the loader's downloaded libraries
	"versions":  true, // the vanilla version cache some loaders keep
	"jars":      true, // *.jar in the sub-server root, non-recursively
}

// validateWipePaths refuses an unknown token rather than dropping it.
//
// Dropping would hand back a dirty update reported as a success - the exact
// failure the feature exists to end, with a confirmation dialog in front of it
// to make it look deliberate.
func validateWipePaths(tokens []string) error {
	for _, t := range tokens {
		if !installWipeTokens[strings.TrimSpace(t)] {
			return fmt.Errorf("unknown wipe target %q", t)
		}
	}
	return nil
}

type SwitchSubServerRequest struct {
	SubServerName string `json:"subServerName"`
}

type PowerActionRequest struct {
	Action string `json:"action"` // "start", "stop", "restart", "kill"
}

// ==========================================
// HANDLER
// ==========================================

type ServerHandler struct {
	d Deps
}

func NewServerHandler(state Deps) *ServerHandler {
	return &ServerHandler{d: state}
}

// GetGatewayPlayers GET /api/servers/players - players connected through the
// gateway, per server id, for the servers the caller's own list shows. The
// edges count them (a play session per stream), so a server on IP:port
// routing, or one nobody is on, is absent. On the managed gateway the
// provider counts them instead (remoteGatewayPlayers), split by edition.
//
// ponytail: one server-list query per poll per open panel; serve it from the
// list the panel already holds if that load ever shows.
func (h *ServerHandler) GetGatewayPlayers(w http.ResponseWriter, r *http.Request) {
	if h.d.Store == nil || h.d.Redis == nil {
		httpx.SendJSONError(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}
	// An unread mode answers as the local-only gate on this route did.
	rg, merr := services.ActiveRemoteGateway(h.d.GatewayRemote, h.d.RemoteGateway)
	if gate.GatewayModeError(w, merr) {
		return
	}
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	userID, _ := r.Context().Value("userID").(string)
	username, _ := r.Context().Value("username").(string)
	servers, err := h.d.Store.ListServersForUser(userID, isAdmin)
	if err != nil {
		httpx.SendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}
	servers = access.ApplyResolvedTabPermissions(h.d.Authz, servers, userID, username)
	if rg != nil {
		h.remoteGatewayPlayers(w, r, rg, servers)
		return
	}
	counts := services.GatewayPlayersByServer(r.Context(), h.d.Redis)
	out := map[string]int{}
	for _, s := range servers {
		if n := counts[s.UUID]; n > 0 {
			out[strconv.Itoa(s.ID)] = n
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"players": out})
}

// playersLogged is when remoteGatewayPlayers last logged a failed route read
// (unix seconds): the sidebar polls every few seconds, the log gets one line a
// minute.
var playersLogged atomic.Int64

type editionCounts struct {
	Java    int `json:"java"`
	Bedrock int `json:"bedrock"`
}

// remoteGatewayPlayers is GetGatewayPlayers on the managed gateway: the
// provider's counts per address (PlayerCounts: cached, never a long wait),
// summed per server through the local remote_routes rows, for the servers in
// the caller's list only. A provider address with no row here counts for
// nobody. Counts that cannot be read are no counts, never an error: the
// sidebar treats both the same. stale and countsAt say how old they are.
func (h *ServerHandler) remoteGatewayPlayers(w http.ResponseWriter, r *http.Request, rg *services.RemoteGateway, servers []models.Server) {
	players, editions := map[string]int{}, map[string]*editionCounts{}
	out := map[string]any{"players": players, "editions": editions, "stale": false}
	rows, err := h.d.Store.ListRemoteRoutes()
	if err != nil {
		if now, last := time.Now().Unix(), playersLogged.Load(); now-last >= 60 && playersLogged.CompareAndSwap(last, now) {
			log.Printf("player counts: remote routes: %v", err)
		}
	} else {
		idByUUID := make(map[string]string, len(servers))
		for _, s := range servers {
			idByUUID[s.UUID] = strconv.Itoa(s.ID)
		}
		serverOf := make(map[string]string, len(rows))
		for _, row := range rows {
			if id, ok := idByUUID[row.ServerUUID]; ok {
				serverOf[row.Domain] = id
			}
		}
		counts := rg.PlayerCounts(r.Context())
		for _, it := range counts.Items {
			id, ok := serverOf[it.Domain]
			if !ok || it.Java+it.Bedrock <= 0 {
				continue
			}
			e := editions[id]
			if e == nil {
				e = &editionCounts{}
				editions[id] = e
			}
			e.Java += it.Java
			e.Bedrock += it.Bedrock
			players[id] += it.Java + it.Bedrock
		}
		out["stale"] = counts.Stale
		if !counts.At.IsZero() {
			out["countsAt"] = counts.At.UTC().Format(time.RFC3339)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// GetServers GET /api/servers - the servers the caller may see: their own plus
// any they were invited to, or the whole fleet for an admin.
func (h *ServerHandler) GetServers(w http.ResponseWriter, r *http.Request) {
	if h.d.Store == nil {
		httpx.SendJSONError(w, "Database not connected", 503)
		return
	}

	isAdmin := r.Context().Value("isAdmin").(bool)
	userID, _ := r.Context().Value("userID").(string)

	servers, err := h.d.Store.ListServersForUser(userID, isAdmin)
	if err != nil {
		httpx.SendJSONError(w, "Database error", 500)
		return
	}

	if servers == nil {
		servers = []models.Server{}
	}

	username, _ := r.Context().Value("username").(string)
	servers = access.ApplyResolvedTabPermissions(h.d.Authz, servers, userID, username)

	// Demo servers. Mark any server already in the list that is on the demo list
	// (so its owner/admin sees the demo status in the toggle), and — for the
	// designated read-only demo account — append the showcase server(s) so the
	// public demo session has something to look at. Read access is enforced
	// server-side; the role "demo" + read-only permission set here only tells the
	// panel to render the appended ones read-only.
	// Whole demo surface only exists on the hosted (store-enabled) build; a
	// self-host build never reads a stale demo_server_uuids setting.
	if demoUUIDs := demo.LoadServerUUIDs(h.d.Store); h.d.StoreEnabled && len(demoUUIDs) > 0 {
		demoSet := make(map[string]bool, len(demoUUIDs))
		for _, u := range demoUUIDs {
			demoSet[u] = true
		}
		for i := range servers {
			if demoSet[servers[i].UUID] {
				servers[i].IsDemo = true
			}
		}
		if demo.IsAccount(h.d.Store, h.d.StoreEnabled, userID) {
			for _, uuid := range demoUUIDs {
				ds, derr := h.d.Store.GetServerByUUID(uuid)
				if derr != nil || ds == nil {
					continue
				}
				ds.IsDemo = true
				ds.Role = "demo"
				ds.Permissions = &models.TabPermissions{Console: true, Files: true}
				access.RedactSettingsFields(ds)
				servers = append(servers, *ds)
			}
		}
	}

	// Say why an install is not moving, rather than leaving a spinner that
	// never resolves. Costs nothing when no server is installing.
	nodeops.AnnotateStalledInstallsFor(r.Context(), h.d.Redis, h.d.GRPCRegistry, servers)

	access.RedactNodeAddress(h.d.Store, func() bool { return services.GatewayRoutingEnabled(h.d.Store) }, servers, isAdmin, userID)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"servers": servers,
	})
}
