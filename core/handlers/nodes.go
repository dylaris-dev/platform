package handlers

import (
	"bytes"
	"database/sql"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
	"dylaris-pkg/validate"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	pb "dylaris-proto/node"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"golang.org/x/crypto/bcrypt"
)

// orphanNameRegex restricts orphan folder names to safe filename chars.
// Permits standard UUIDs and legacy folder names (e.g. "test-server-1")
// while rejecting path traversal attempts ('/', '..', etc.).
var orphanNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func isSafeOrphanName(name string) bool {
	return orphanNameRegex.MatchString(name)
}

type NodeHandler struct {
	state *AppState
}

func NewNodeHandler(state *AppState) *NodeHandler {
	return &NodeHandler{state: state}
}

// nodeExists reports whether the node row is still there. The four orphan
// endpoints below take a nodeId straight from the path and went on to the
// gRPC call, so a node id that never existed came back as 502 "node N not
// connected" - which sends an operator looking for a connectivity fault on a
// node that was deleted, or never was. A missing row is a 404; "not
// connected" stays reserved for a node that exists and is offline.
func (h *NodeHandler) nodeExists(nodeID int) bool {
	if h.state.Store == nil {
		return false
	}
	_, err := h.state.Store.GetNodeByID(nodeID)
	return err == nil
}

// The infrastructure page has one tab per KIND of machine, and these are the
// two kinds it can name. What is left over - a swarm host, the operator's own
// in-cluster hardware - belongs on neither and stays on /infrastructure.
//
// owner_id is the reliable half: it is written at enroll time and nothing else
// sets it. The external tag is reported by the node itself; before 2026-08-19
// no external node reported it at all, so a machine that has not reconnected
// since then is missing from the external tab until it does.
// Both delegate to models.Node.Kind so the scope filter, the placement rules
// and the metrics collector cannot drift into three answers to one question.
func isExternalPlatformNode(n models.Node) bool { return n.Kind() == models.NodeKindExternal }
func isBYONNode(n models.Node) bool             { return n.Kind() == models.NodeKindBYON }

// ownedBYONNode is the "Your machines" predicate: a BYON node belonging to this
// caller. An empty caller matches nothing, so a request that arrived without an
// identity gets an empty list rather than the fleet.
func ownedBYONNode(uid string) func(models.Node) bool {
	return func(n models.Node) bool { return isBYONNode(n) && uid != "" && *n.OwnerID == uid }
}

// placeableNode is the PLACEMENT predicate: a node this caller may be offered
// as a target. An unowned node (platform or the operator's own external
// hardware, the machines that hold CLUSTER_SECRET) is offered to any admin; an
// owned node is offered only to its owner.
//
// It exists so the picker shows exactly what canPlaceOnNode will accept. A list
// that offers a target the create call then refuses is a worse answer than a
// shorter list.
func placeableNode(uid string, admin, byon bool) func(models.Node) bool {
	return func(n models.Node) bool {
		if n.OwnerID != nil && byon {
			return uid != "" && *n.OwnerID == uid
		}
		return admin
	}
}

// filterNodes keeps the nodes keep() accepts, preserving order. Returns an
// empty slice rather than nil so the JSON stays [] and never null.
func filterNodes(nodes []models.Node, keep func(models.Node) bool) []models.Node {
	out := make([]models.Node, 0, len(nodes))
	for _, n := range nodes {
		if keep(n) {
			out = append(out, n)
		}
	}
	return out
}

// GetNodes GET /api/nodes - the machine list. Admin-only except in BYON mode,
// where a tenant sees only the nodes they own. ?scope=external|byon narrows it
// to one kind of machine, and that is enforced here rather than in the panel:
// a tab a non-admin can reach by editing the URL is not admin-only in any
// sense that matters.
func (h *NodeHandler) GetNodes(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "DB error", 503)
		return
	}
	// Node list is admin-only today. In BYON mode a non-admin tenant may list
	// THEIR OWN nodes; everything else stays admin-only.
	admin := IsAdmin(r)
	byon := byonActive(h.state, r)
	if !admin && !byon {
		sendJSONError(w, "Forbidden", 403)
		return
	}

	nodes, err := h.state.Store.ListNodes()
	if err != nil {
		sendJSONError(w, "Failed to load nodes", 500)
		return
	}
	if nodes == nil {
		nodes = []models.Node{}
	}

	// BYON tenant: scope the list to nodes they own.
	if !admin && byon {
		uid := byonCallerID(r)
		owned := make([]models.Node, 0, len(nodes))
		for _, n := range nodes {
			if n.OwnerID != nil && *n.OwnerID == uid {
				owned = append(owned, n)
			}
		}
		nodes = owned
	}

	// ?scope= narrows the list to one KIND of machine. The infrastructure page
	// has a tab per kind and an admin sees all three, so without this an admin
	// got the whole fleet - swarm hosts included - under "my machines".
	//
	// Enforced here rather than filtered in the panel: the external tab is the
	// operator's own machines, and a tab a non-admin can reach by editing the
	// URL is not admin-only in any sense that matters.
	if scope := strings.TrimSpace(r.URL.Query().Get("scope")); scope != "" {
		switch scope {
		case "external":
			if !admin {
				sendJSONError(w, "Forbidden", 403)
				return
			}
			nodes = filterNodes(nodes, isExternalPlatformNode)
		case "byon":
			// Owned by the CALLER, not merely of the BYON kind. The tab this
			// answers is titled "Your machines", and an admin asking for it got
			// every tenant's machine in the fleet under that heading - the
			// customer's box at home listed as the operator's own.
			//
			// Scoped for admins too, which is the part that was missing: the
			// non-admin branch above already narrows by owner, so the kind
			// filter here looked like it finished the job while only the
			// unprivileged half of it was ever enforced. An admin who needs the
			// whole fleet asks for the unscoped list, which is still theirs.
			nodes = filterNodes(nodes, ownedBYONNode(byonCallerID(r)))
		case "placement":
			// What may I put a server on. Answers the target-node picker in the
			// create wizard and the transfer dialog, and deliberately hides
			// another tenant's machine from an operator: their hardware is not
			// the operator's capacity, and canPlaceOnNode refuses it anyway.
			nodes = filterNodes(nodes, placeableNode(byonCallerID(r), admin, byon))
		case "fleet":
			// The machines the OPERATOR runs: platform and external, never a
			// customer's. It backs Settings -> Nodes, which offers Configure,
			// Reset pairing and the deploy bundle - none of which an operator
			// has any business doing to hardware somebody else owns.
			//
			// That screen used to ask for the unscoped list, so every tenant's
			// machine appeared there with those buttons live next to it.
			if !admin {
				sendJSONError(w, "Forbidden", 403)
				return
			}
			nodes = filterNodes(nodes, func(n models.Node) bool { return !isBYONNode(n) })
		default:
			sendJSONError(w, "Unknown scope", 400)
			return
		}
	}

	// The live half of a node, from its heartbeat. ListNodes answers from the
	// nodes TABLE, and everything a running node reports about itself has no
	// column - so on this endpoint those fields were whatever the zero value
	// is, for every node, always.
	//
	// It went unnoticed while nothing on this screen displayed one. Isolation
	// was the first, and it arrived reading `false` on every node in the fleet:
	// the badge could only ever say "shared network", including about nodes
	// that were isolating perfectly well. Enriched here, after the scope filter
	// so the Redis reads cover only the nodes actually being returned.
	//
	// Shared with the infrastructure page and the metrics collector rather than
	// done a third way here - that function exists because the first two had
	// already drifted.
	services.EnrichNodesWithLiveStats(r.Context(), h.state.Store, h.state.Redis, nodes)

	// Derive the unusable flag at response time (no DB column needed): an
	// external/home node only routes via gateway+beam, so while the platform
	// is in ip_port mode it has no reachable path. Panel uses this to show a
	// "requires gateway" badge instead of letting admins place servers there.
	gatewayOn := h.state.gatewayEnabled()
	for i := range nodes {
		if nodes[i].IsExternal() && !gatewayOn {
			nodes[i].Unusable = true
			nodes[i].UnusableReason = "requires_gateway"
		}
		// A node with no region was booted without NODE_REGION - flag it so the
		// panel can say so. The fix is on the node, in its environment: nothing
		// in the panel sets a region any more.
		if nodes[i].Region == "" {
			nodes[i].NeedsConfiguration = true
		}
	}

	// FIX: Return as object
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"nodes":   nodes,
	})
}

// CreateNode POST /api/nodes - registers a node and mints its token. An
// omitted address defaults to 0.0.0.0.
func (h *NodeHandler) CreateNode(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "DB error", 503)
		return
	}

	var req models.Node
	json.NewDecoder(r.Body).Decode(&req)

	req.Token = uuid.New().String()
	if req.Address == "" {
		req.Address = "0.0.0.0"
	}
	req.Status = "offline"
	req.CreatedAt = time.Now()

	if err := h.state.Store.CreateNode(&req); err != nil {
		sendJSONError(w, "Failed to create node", 500)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "message": "Node created", "node": req,
	})
}

// UpdateNode PUT /api/nodes/{id} - edits a node. Today that is cpusetCpus
// alone, the CPU pinning mask.
func (h *NodeHandler) UpdateNode(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "DB error", 503)
		return
	}

	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	var req struct {
		CpusetCpus *string `json:"cpusetCpus"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	if req.CpusetCpus != nil {
		if err := h.state.Store.UpdateNodeCpusetCpus(id, *req.CpusetCpus); err != nil {
			sendJSONError(w, "Failed to update node", 500)
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "message": "Node updated",
	})
}

// DeleteNode DELETE /api/nodes/{id} - removes a node, then cleans up the Redis
// ACL user and keys that belong to it. Those are all keyed by the node token,
// which is why the row is read before it is deleted; if that read fails the
// row still goes and the leftovers are logged rather than silently abandoned.
func (h *NodeHandler) DeleteNode(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "DB error", 503)
		return
	}

	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	// Read the node BEFORE the row goes: everything below is keyed by its token.
	node, nodeErr := h.state.Store.GetNodeByID(id)

	if err := h.state.Store.DeleteNode(id); err != nil {
		// Not a fault, the current state of the data: say what to do about it.
		// Collapsing it into a 500 left an admin with a button that does nothing
		// and no hint that the servers have to go first.
		if errors.Is(err, store.ErrNodeHasServers) {
			sendJSONError(w, "This node still has servers on it. Move or delete them first, or use force-delete to remove the node and its servers together.", 409)
			return
		}
		sendJSONError(w, "Delete failed", 500)
		return
	}

	if nodeErr == nil && node != nil {
		h.cleanupDeletedNode(r, node.Token)
	} else {
		log.Printf("node delete: could not read node %d before deleting it, so its Redis ACL and keys were left behind: %v", id, nodeErr)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// cleanupDeletedNode drops the Redis state a deleted node leaves behind.
//
// The ACL half is the one that matters. Deleting the row alone left all three
// scoped Redis users live, and a node caches its secret in dylaris_data (it
// survives a container recreate on purpose), so removing a node from the panel
// did NOT revoke its Redis access - it kept authenticating with the credential
// it already had. RemoveNodeACL's own doc names this as its case ("e.g. node
// deleted"); the only caller was the pairing-reset path in node_admission.go.
// Same hard cut here: ACL DELUSER disconnects live clients rather than waiting
// for a reconnect that a deleted node has no reason to make.
//
// The keys are housekeeping: none of them expire and nothing else removes them,
// so every deleted node left a few behind for good.
//
// Both are best-effort. The row is already gone and the request has succeeded;
// a Redis hiccup here must not turn a completed delete into a 500.
func (h *NodeHandler) cleanupDeletedNode(r *http.Request, token string) {
	if h.state.Redis == nil || token == "" {
		return
	}
	// One implementation, because there are two callers: this, and the account
	// teardown that removes the nodes a departing tenant brought. A second
	// copy of the key list is how one of them ends up missing a key.
	services.RemoveNodeRedisState(r.Context(), h.state.Redis, redisacl.NewProvisioner(h.state.Redis), token)
}

// GetNodeServers returns all servers assigned to a node
func (h *NodeHandler) GetNodeServers(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	node, err := h.state.Store.GetNodeByID(id)
	if err != nil || node == nil {
		sendJSONError(w, "Node not found", 404)
		return
	}
	if !canManageNode(h.state, r, node) {
		sendJSONError(w, "Forbidden", 403)
		return
	}

	servers, err := h.state.Store.ListServersByNode(id)
	if err != nil {
		sendJSONError(w, "Failed to load servers", 500)
		return
	}
	if servers == nil {
		servers = []models.Server{}
	}
	// This is the view an operator opens when a node looks wrong, so it is the
	// one place a stalled install most needs to be visible.
	annotateStalledInstallsFor(r.Context(), h.state, servers)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"servers": servers,
	})
}

// ForceDeleteNode deletes an offline node and all its servers
func (h *NodeHandler) ForceDeleteNode(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	node, err := h.state.Store.GetNodeByID(id)
	if err != nil {
		sendJSONError(w, "Node not found", 404)
		return
	}

	if node.Status == "online" {
		sendJSONError(w, "Cannot force-delete an online node", 400)
		return
	}

	servers, _ := h.state.Store.ListServersByNode(id)

	// Delete all servers on this node first (FK constraint)
	if err := h.state.Store.DeleteServersByNode(id); err != nil {
		sendJSONError(w, "Failed to delete servers on node", 500)
		return
	}

	if err := h.state.Store.DeleteNode(id); err != nil {
		sendJSONError(w, "Failed to delete node", 500)
		return
	}

	deletedNames := make([]string, 0, len(servers))
	for _, s := range servers {
		deletedNames = append(deletedNames, s.Name)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":        true,
		"deletedServers": deletedNames,
	})
}

// GetNodeStorage returns storage path info from the node's Redis heartbeat.
// GET /api/nodes/{id}/storage
func (h *NodeHandler) GetNodeStorage(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	node, err := h.state.Store.GetNodeByID(id)
	if err != nil || node == nil {
		sendJSONError(w, "Node not found", 404)
		return
	}
	if !canManageNode(h.state, r, node) {
		sendJSONError(w, "Forbidden", 403)
		return
	}

	if h.state.Redis == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"storage": []interface{}{},
		})
		return
	}

	key := "dylaris:discovery:" + node.Token
	val, err := h.state.Redis.Get(r.Context(), key).Result()
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"storage": []interface{}{},
			"message": "Node heartbeat not found (offline?)",
		})
		return
	}

	var heartbeat map[string]interface{}
	if err := json.Unmarshal([]byte(val), &heartbeat); err != nil {
		sendJSONError(w, "Failed to parse heartbeat", 500)
		return
	}

	storage, ok := heartbeat["storage"]
	if !ok {
		storage = []interface{}{}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"storage":   storage,
		"placement": h.readStoragePlacement(r, node.Token),
		"capacity":  h.storageCapacity(r, node),
	})
}

// storageCapacity is the per-path picture the panel shows: what is free, how
// much of it is already promised to existing servers, and what that leaves.
// Empty when the node is offline or reported no storage.
func (h *NodeHandler) storageCapacity(r *http.Request, node *models.Node) []services.DiskPathStatus {
	if h.state.Redis == nil {
		return []services.DiskPathStatus{}
	}
	hb := services.LoadHeartbeat(r.Context(), h.state.Redis, node.Token)
	if hb == nil || len(hb.Storage) == 0 {
		return []services.DiskPathStatus{}
	}
	limits, err := h.state.Store.ServerDiskLimitsByNode(node.ID)
	if err != nil {
		limits = nil
	}
	warn, critical := services.DiskThresholds(h.state.Store)
	return services.PathStatuses(
		hb.Storage,
		services.CommittedBytesByPath(hb.Storage, limits),
		services.DiskHeadroomGB(h.state.Store),
		warn, critical,
	)
}

// storagePlacementKey is per NODE, not global: the paths differ per node, so a
// single fleet-wide order could not name them.
func storagePlacementKey(nodeToken string) string {
	return "dylaris:node:" + nodeToken + ":storage_placement"
}

// storagePlacement is the policy for placing NEW servers on a node.
// Mode "auto" picks the path with the most free space (the historical
// behaviour); "manual" walks Order and takes the first usable path.
type storagePlacement struct {
	Mode  string   `json:"mode"`
	Order []string `json:"order"`
}

// readStoragePlacement returns the stored policy, defaulting to auto. A missing
// or unreadable key is not an error: auto is what the node does anyway.
func (h *NodeHandler) readStoragePlacement(r *http.Request, nodeToken string) storagePlacement {
	out := storagePlacement{Mode: "auto", Order: []string{}}
	if h.state.Redis == nil {
		return out
	}
	val, err := h.state.Redis.Get(r.Context(), storagePlacementKey(nodeToken)).Result()
	if err != nil || val == "" {
		return out
	}
	var stored storagePlacement
	if json.Unmarshal([]byte(val), &stored) != nil {
		return out
	}
	if stored.Mode != "manual" {
		stored.Mode = "auto"
	}
	if stored.Order == nil {
		stored.Order = []string{}
	}
	return stored
}

// SetNodeStoragePlacement PUT /api/nodes/{id}/storage-placement
// Sets how the node places NEW servers across its storage paths. Existing
// servers never move: their path is pinned in Redis at creation time.
func (h *NodeHandler) SetNodeStoragePlacement(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	node, err := h.state.Store.GetNodeByID(id)
	if err != nil || node == nil {
		sendJSONError(w, "Node not found", 404)
		return
	}
	if !canManageNode(h.state, r, node) {
		sendJSONError(w, "Forbidden", 403)
		return
	}
	if h.state.Redis == nil {
		sendJSONError(w, "Redis unavailable", 503)
		return
	}

	var req storagePlacement
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request body", 400)
		return
	}
	if req.Mode != "auto" && req.Mode != "manual" {
		sendJSONError(w, "mode must be auto or manual", 400)
		return
	}

	// Only keep paths this node actually reports, deduped and in the submitted
	// order. Rejecting instead would strand an admin whose node dropped a disk;
	// dropping instead would hide a typo. So: reject what the node knows to be
	// wrong, accept everything when the node is offline and cannot tell us.
	known := h.nodeStoragePaths(r, node.Token)
	order := make([]string, 0, len(req.Order))
	seen := make(map[string]bool, len(req.Order))
	for _, p := range req.Order {
		if p == "" || seen[p] {
			continue
		}
		if len(known) > 0 && !known[p] {
			sendJSONError(w, "unknown storage path: "+p, 400)
			return
		}
		seen[p] = true
		order = append(order, p)
	}

	payload, err := json.Marshal(storagePlacement{Mode: req.Mode, Order: order})
	if err != nil {
		sendJSONError(w, "Failed to encode placement", 500)
		return
	}
	// No TTL: this is configuration, and the node re-reads it on every settings
	// poll. It survives a node restart but not a Redis flush - same as the other
	// dylaris:placement:* settings.
	if err := h.state.Redis.Set(r.Context(), storagePlacementKey(node.Token), payload, 0).Err(); err != nil {
		sendJSONError(w, "Failed to store placement", 500)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"placement": storagePlacement{Mode: req.Mode, Order: order},
	})
}

// GetFleetStoragePlacement GET /api/settings/storage-placement
// The fleet-wide default a node uses when it has no policy of its own, plus
// every storage path any node currently reports (no single node's list
// describes the fleet).
func (h *NodeHandler) GetFleetStoragePlacement(w http.ResponseWriter, r *http.Request) {
	if h.state.Redis == nil {
		sendJSONError(w, "Redis unavailable", 503)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"placement": services.LoadFleetPlacement(r.Context(), h.state.Redis),
		"paths":     services.FleetStoragePaths(r.Context(), h.state.Redis),
	})
}

// SetFleetStoragePlacement PUT /api/settings/storage-placement - sets fleet-
// wide storage placement to auto or manual, manual carrying an explicit node
// order. Stored in Redis, not in the database.
func (h *NodeHandler) SetFleetStoragePlacement(w http.ResponseWriter, r *http.Request) {
	if h.state.Redis == nil {
		sendJSONError(w, "Redis unavailable", 503)
		return
	}
	var req storagePlacement
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request body", 400)
		return
	}
	if req.Mode != "auto" && req.Mode != "manual" {
		sendJSONError(w, "mode must be auto or manual", 400)
		return
	}

	// Unlike the per-node form, an unknown path is NOT an error here: the fleet
	// default names paths that only some nodes have, and a node that lacks one
	// simply skips it. Rejecting would make the setting unusable on a mixed fleet.
	order := make([]string, 0, len(req.Order))
	seen := make(map[string]bool, len(req.Order))
	for _, p := range req.Order {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		order = append(order, p)
	}

	next := services.NodePlacement{Mode: req.Mode, Order: order}
	if err := services.SaveFleetPlacement(r.Context(), h.state.Redis, next); err != nil {
		sendJSONError(w, "Failed to store placement", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "placement": next})
}

// nodeStoragePaths returns the paths the node last reported, or an empty map
// when it is offline or has not reported any.
func (h *NodeHandler) nodeStoragePaths(r *http.Request, nodeToken string) map[string]bool {
	out := map[string]bool{}
	if h.state.Redis == nil {
		return out
	}
	val, err := h.state.Redis.Get(r.Context(), "dylaris:discovery:"+nodeToken).Result()
	if err != nil {
		return out
	}
	var hb struct {
		Storage []struct {
			Path string `json:"path"`
		} `json:"storage"`
	}
	if json.Unmarshal([]byte(val), &hb) != nil {
		return out
	}
	for _, s := range hb.Storage {
		if s.Path != "" {
			out[s.Path] = true
		}
	}
	return out
}

// GetDeployBundle GET /api/nodes/{id}/deploy-bundle returns the values a secret-free
// BYON host needs: the gRPC-TLS pin fingerprint plus the node's Link tunnel token and
// discovery proof (both Core-derived from CLUSTER_SECRET, so Link never holds it).
func (h *NodeHandler) GetDeployBundle(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	node, err := h.state.Store.GetNodeByID(id)
	if err != nil || node == nil {
		sendJSONError(w, "Node not found", 404)
		return
	}
	if !canManageNode(h.state, r, node) {
		sendJSONError(w, "Forbidden", 403)
		return
	}

	// The fingerprint is only meaningful pinning material when TLS is actually
	// on; showing it while GRPC_TLS_ENABLED=false would suggest a pin the
	// control channel never enforces.
	fingerprint := ""
	if h.state.GRPCTLSEnabled {
		fingerprint = h.state.GRPCTLSFingerprint
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":            true,
		"nodeId":             node.Token,
		"grpcTlsFingerprint": fingerprint,
		"linkSecret":         h.state.Gateway.LinkToken(node.Token),
		"linkDiscoveryProof": h.state.Gateway.DiscoveryProof(node.Token),
	})
}

// storageHeartbeatEntry is used to parse storage entries from the node discovery heartbeat.
type storageHeartbeatEntry struct {
	Path        string   `json:"path"`
	ServerUUIDs []string `json:"server_uuids"`
}

// GetDiskAnalysis cross-references disk folders on a node with DB servers.
// GET /api/admin/nodes/{id}/disk-analysis
func (h *NodeHandler) GetDiskAnalysis(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	node, err := h.state.Store.GetNodeByID(id)
	if err != nil || node == nil {
		sendJSONError(w, "Node not found", 404)
		return
	}

	dbServers, err := h.state.Store.ListServersByNode(id)
	if err != nil {
		sendJSONError(w, "Failed to load servers", 500)
		return
	}

	dbByUUID := make(map[string]models.Server)
	for _, s := range dbServers {
		dbByUUID[s.UUID] = s
	}

	diskUUIDs := make(map[string]bool)
	nodeOnline := false
	if h.state.Redis != nil {
		key := "dylaris:discovery:" + node.Token
		if val, redisErr := h.state.Redis.Get(r.Context(), key).Result(); redisErr == nil {
			nodeOnline = true
			var hb struct {
				Storage []storageHeartbeatEntry `json:"storage"`
			}
			if json.Unmarshal([]byte(val), &hb) == nil {
				for _, entry := range hb.Storage {
					for _, u := range entry.ServerUUIDs {
						diskUUIDs[u] = true
					}
				}
			}
		}
	}

	type matchedEntry struct {
		UUID   string `json:"uuid"`
		Name   string `json:"serverName"`
		Owner  string `json:"ownerName"`
		Status string `json:"status"`
	}
	type orphanedEntry struct {
		UUID string `json:"uuid"`
	}
	type missingEntry struct {
		ID   int    `json:"id"`
		UUID string `json:"uuid"`
		Name string `json:"serverName"`
	}

	matched := []matchedEntry{}
	orphaned := []orphanedEntry{}
	missing := []missingEntry{}

	for u := range diskUUIDs {
		if srv, ok := dbByUUID[u]; ok {
			matched = append(matched, matchedEntry{UUID: u, Name: srv.Name, Owner: srv.OwnerName, Status: srv.Status})
		} else {
			orphaned = append(orphaned, orphanedEntry{UUID: u})
		}
	}
	// Only compute the missing list against an online node — an offline node has no
	// fresh disk view and would mislabel every server as missing.
	if nodeOnline {
		for u, srv := range dbByUUID {
			if !diskUUIDs[u] {
				missing = append(missing, missingEntry{ID: srv.ID, UUID: u, Name: srv.Name})
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"nodeOnline": nodeOnline,
		"matched":    matched,
		"orphaned":   orphaned,
		"missing":    missing,
	})
}

// DeleteOrphanedFolder deletes an orphaned UUID folder from a node via gRPC.
// DELETE /api/admin/nodes/{id}/orphan?uuid=
func (h *NodeHandler) DeleteOrphanedFolder(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	nodeID, _ := strconv.Atoi(vars["id"])

	orphanUUID := r.URL.Query().Get("uuid")
	if orphanUUID == "" {
		sendJSONError(w, "uuid query param required", 400)
		return
	}
	// Path-traversal guard: orphan folder names must be safe filename chars only.
	// Standard UUIDs match, but so do legacy/test folders (e.g. "test-server-1").
	// We don't strictly require UUIDv4 since orphans are scanned from disk and
	// may have non-UUID names from manual setups.
	if !isSafeOrphanName(orphanUUID) {
		sendJSONError(w, "invalid uuid: only a-z, A-Z, 0-9, '-' and '_' allowed, max 64 chars", 400)
		return
	}
	if !h.nodeExists(nodeID) {
		sendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}

	// Safety check: ensure it is NOT in the DB
	srv, _ := h.state.Store.GetServerByUUID(orphanUUID)
	if srv != nil {
		sendJSONError(w, "Server exists in database — use normal delete", 400)
		return
	}

	if h.state.GRPCRegistry == nil {
		sendJSONError(w, "gRPC not available", 503)
		return
	}

	resp, err := h.state.GRPCRegistry.SendRequest(nodeID, &pb.NodeMessage{
		RequestId:  fmt.Sprintf("orphan-del-%s", orphanUUID),
		ServerUuid: orphanUUID,
		Payload:    &pb.NodeMessage_DeleteReq{DeleteReq: &pb.DeleteFileReq{Path: "."}},
	}, 30*time.Second)
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Node communication error: %v", err), 502)
		return
	}
	if errResp := resp.GetError(); errResp != nil {
		sendJSONError(w, errResp.Message, 500)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Orphaned folder %s deleted", orphanUUID),
	})
}

// ListOrphanFiles lists the files inside an orphaned folder on a node.
// GET /api/disk/orphans/{nodeId}/{uuid}/files?path=...
// Admin-only, read-only. No DB servers row required.
func (h *NodeHandler) ListOrphanFiles(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	nodeID, _ := strconv.Atoi(vars["nodeId"])
	orphanUUID := vars["uuid"]

	if !isSafeOrphanName(orphanUUID) {
		sendJSONError(w, "invalid uuid: only a-z, A-Z, 0-9, '-' and '_' allowed, max 64 chars", 400)
		return
	}
	if !h.nodeExists(nodeID) {
		sendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}

	pathParam := r.URL.Query().Get("path")
	// Reject any path containing ".." or starting with "/" to prevent traversal outside the orphan root.
	if strings.Contains(pathParam, "..") || (len(pathParam) > 0 && pathParam[0] == '/') {
		sendJSONError(w, "Path must not contain '..' or start with '/'", 400)
		return
	}
	// Default to root when path is empty.
	if pathParam == "" {
		pathParam = "/"
	}

	if h.state.GRPCRegistry == nil {
		sendJSONError(w, "gRPC not available", 503)
		return
	}

	resp, err := h.state.GRPCRegistry.SendRequest(nodeID, &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: orphanUUID,
		Payload:    &pb.NodeMessage_ListReq{ListReq: &pb.ListFilesReq{Path: pathParam}},
	}, 30*time.Second)
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Node communication error: %v", err), 502)
		return
	}
	if errResp := resp.GetError(); errResp != nil {
		sendJSONError(w, errResp.Message, int(errResp.Code))
		return
	}

	listResp := resp.GetListResp()
	if listResp == nil {
		sendJSONError(w, "Unexpected response from node", 500)
		return
	}

	type fileEntry struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	}
	files := make([]fileEntry, 0, len(listResp.Files))
	for _, f := range listResp.Files {
		files = append(files, fileEntry{Name: f.Name, IsDir: f.IsDir, Size: f.Size})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"files":   files,
	})
}

// GetOrphanFileContent returns the text content of a file inside an orphaned folder.
// GET /api/disk/orphans/{nodeId}/{uuid}/content?path=...
// Admin-only, read-only. No DB servers row required.
func (h *NodeHandler) GetOrphanFileContent(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	nodeID, _ := strconv.Atoi(vars["nodeId"])
	orphanUUID := vars["uuid"]

	if !isSafeOrphanName(orphanUUID) {
		sendJSONError(w, "invalid uuid: only a-z, A-Z, 0-9, '-' and '_' allowed, max 64 chars", 400)
		return
	}
	if !h.nodeExists(nodeID) {
		sendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}

	pathParam := r.URL.Query().Get("path")
	if strings.Contains(pathParam, "..") || (len(pathParam) > 0 && pathParam[0] == '/') {
		sendJSONError(w, "Path must not contain '..' or start with '/'", 400)
		return
	}
	if pathParam == "" {
		sendJSONError(w, "path query param required", 400)
		return
	}

	if h.state.GRPCRegistry == nil {
		sendJSONError(w, "gRPC not available", 503)
		return
	}

	reqID := uuid.NewString()
	msg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: orphanUUID,
		Payload: &pb.NodeMessage_ReadReq{
			ReadReq: &pb.ReadFileReq{Path: pathParam, ZipIfDir: false},
		},
	}

	ch, err := h.state.GRPCRegistry.SendRequestStreaming(nodeID, msg)
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Node communication error: %v", err), 502)
		return
	}
	defer h.state.GRPCRegistry.CleanupRequest(nodeID, reqID)

	var buf bytes.Buffer
	for resp := range ch {
		if errResp := resp.GetError(); errResp != nil {
			sendJSONError(w, errResp.Message, int(errResp.Code))
			return
		}
		if chunk := resp.GetChunk(); chunk != nil {
			buf.Write(chunk.Data)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"content": buf.String(),
	})
}

// AssignOrphanRequest is the body for POST /api/disk/orphans/assign.
type AssignOrphanRequest struct {
	NodeID      int     `json:"node_id"`
	UUID        string  `json:"uuid"`
	Name        string  `json:"name"`
	OwnerUserID *string `json:"owner_user_id"` // existing user, or nil
	NewUser     *struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"new_user"` // create a new user, or nil
	MemoryMB int     `json:"memory_mb"`
	CPULimit float64 `json:"cpu_limit"`
}

// dylarisMetadata mirrors the relevant fields of .dylaris.json on disk.
type dylarisMetadata struct {
	Name            string  `json:"name"`
	MemoryMB        int     `json:"memory_mb"`
	CPULimit        float64 `json:"cpu_limit"`
	ActiveSubServer string  `json:"active_sub_server"`
	SubServers      []struct {
		Name             string `json:"name"`
		Type             string `json:"type"`
		MinecraftVersion string `json:"minecraft_version"`
		Build            string `json:"build"`
	} `json:"sub_servers"`
}

// inspectOrphanOnNode sends an inspect_orphan gRPC request to nodeID for orphanUUID
// and returns the parsed response. Returns nil, error on gRPC failure or missing orphan.
// Package-level (not a NodeHandler method) so the server-lifecycle switch path can
// reuse it to refresh a server's loader metadata from the node's .dylaris.json.
func inspectOrphanOnNode(state *AppState, nodeID int, orphanUUID string) (*pb.InspectOrphanResp, error) {
	resp, err := state.GRPCRegistry.SendRequest(nodeID, &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: orphanUUID,
		Payload:    &pb.NodeMessage_InspectOrphanReq{InspectOrphanReq: &pb.InspectOrphanReq{}},
	}, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("node communication error: %w", err)
	}
	if errResp := resp.GetError(); errResp != nil {
		code := int(errResp.Code)
		if code == 0 {
			code = 500
		}
		return nil, fmt.Errorf("node error %d: %s", code, errResp.Message)
	}
	orphanInfo := resp.GetInspectOrphanResp()
	if orphanInfo == nil {
		return nil, fmt.Errorf("orphan folder not found or inspect failed")
	}
	return orphanInfo, nil
}

// subServerLoaderFromInspect extracts one sub-server's loader/version/build from
// a node's inspect_orphan response. It prefers the richer .dylaris.json metadata
// (which carries minecraft_version and build) and falls back to the directory
// scan, which reports the type only. ok reports whether the sub-server was found
// at all, so callers can leave existing values untouched instead of wiping them
// when the node cannot describe the sub-server.
//
// Shared by orphan adoption (first fill) and the sub-server switch (refresh), so
// both derive installer_type/minecraft_version/build_number the same way and
// cannot drift apart.
func subServerLoaderFromInspect(resp *pb.InspectOrphanResp, subName string) (instType, mcVer, build string, ok bool) {
	if resp == nil || subName == "" {
		return "", "", "", false
	}
	if resp.HasMetadata && resp.MetadataJson != "" {
		var meta dylarisMetadata
		if err := json.Unmarshal([]byte(resp.MetadataJson), &meta); err == nil {
			for _, ss := range meta.SubServers {
				if ss.Name == subName {
					return ss.Type, ss.MinecraftVersion, ss.Build, true
				}
			}
		}
	}
	// Fall back to the directory scan when metadata is absent or omits this
	// sub-server. SubServerInfo carries no minecraft_version/build, so those
	// stay empty.
	for _, ss := range resp.SubServers {
		if ss.Name == subName {
			return ss.Type, "", "", true
		}
	}
	return "", "", "", false
}

// InspectOrphan returns metadata about an orphaned folder without assigning it.
// GET /api/disk/orphans/{nodeId:[0-9]+}/{uuid}/inspect  (admin-only)
func (h *NodeHandler) InspectOrphan(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	nodeID, _ := strconv.Atoi(vars["nodeId"])
	orphanUUID := vars["uuid"]

	if !isSafeOrphanName(orphanUUID) {
		sendJSONError(w, "invalid uuid: only a-z, A-Z, 0-9, '-' and '_' allowed, max 64 chars", 400)
		return
	}
	if !h.nodeExists(nodeID) {
		sendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}

	if h.state.GRPCRegistry == nil {
		sendJSONError(w, "gRPC not available", 503)
		return
	}

	orphanInfo, err := inspectOrphanOnNode(h.state, nodeID, orphanUUID)
	if err != nil {
		sendJSONError(w, err.Error(), 502)
		return
	}

	// Parse metadata JSON server-side so the panel receives a clean object.
	var metadataObj interface{}
	if orphanInfo.HasMetadata && orphanInfo.MetadataJson != "" {
		var meta dylarisMetadata
		if jsonErr := json.Unmarshal([]byte(orphanInfo.MetadataJson), &meta); jsonErr == nil {
			metadataObj = meta
		}
	}

	type subServerInfo struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	subServers := make([]subServerInfo, 0, len(orphanInfo.SubServers))
	for _, ss := range orphanInfo.SubServers {
		subServers = append(subServers, subServerInfo{Name: ss.Name, Type: ss.Type})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"metadata":          metadataObj,
		"active_sub_server": orphanInfo.ActiveSubServer,
		"sub_servers":       subServers,
	})
}

// AssignOrphan adopts an on-disk orphan folder into a proper DB server row.
// POST /api/disk/orphans/assign  (admin-only)
//
// It:
//  1. Validates the request.
//  2. Rejects if a servers row for the UUID already exists (409).
//  3. Optionally creates a new owner user (bcrypt password).
//  4. Asks the node to inspect the orphan to discover the active sub-server
//     and its installer type / minecraft version.
//  5. Inserts the servers row (status=stopped) + calls UpdateServerSetup to
//     persist installer_type, minecraft_version, active_sub_server.
//  6. Returns the created server as JSON.
func (h *NodeHandler) AssignOrphan(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "DB error", 503)
		return
	}

	var req AssignOrphanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON body", 400)
		return
	}

	// --- Input validation ---
	if !isSafeOrphanName(req.UUID) {
		sendJSONError(w, "invalid uuid: only a-z, A-Z, 0-9, '-' and '_' allowed, max 64 chars", 400)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		sendJSONError(w, "name is required", 400)
		return
	}
	if req.MemoryMB <= 0 {
		sendJSONError(w, "memory_mb must be > 0", 400)
		return
	}
	if req.NodeID <= 0 {
		sendJSONError(w, "node_id is required", 400)
		return
	}
	// Exactly one owner source must be provided.
	if req.OwnerUserID == nil && req.NewUser == nil {
		sendJSONError(w, "exactly one of owner_user_id or new_user must be set", 400)
		return
	}
	if req.OwnerUserID != nil && req.NewUser != nil {
		sendJSONError(w, "exactly one of owner_user_id or new_user must be set, not both", 400)
		return
	}
	if req.NewUser != nil {
		if strings.TrimSpace(req.NewUser.Username) == "" || req.NewUser.Password == "" {
			sendJSONError(w, "new_user.username and new_user.password are required", 400)
			return
		}
	}

	// --- Duplicate check ---
	//
	// "No such server" is the ONLY state a genuine orphan can be in - that is
	// what makes it an orphan - and GetServerByUUID reports it as
	// sql.ErrNoRows. Treating every error as a database fault therefore turned
	// the one path this endpoint exists to serve into a 500, and adopting an
	// orphan back into the panel could never succeed. The 409 below was
	// reachable, which is why the check looked like it worked.
	//
	// A real fault must still be a fault, not a silent "does not exist": that
	// would let an adopt proceed against a database that could not answer, on
	// a UUID that might well already have a row. Same distinction
	// AuthMiddleware makes between a deleted account and an unreachable store.
	existing, err := h.state.Store.GetServerByUUID(req.UUID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		sendJSONError(w, "database error checking existing server", 500)
		return
	}
	if existing != nil {
		sendJSONError(w, "A server with this UUID already exists in the database", 409)
		return
	}

	// --- gRPC availability check (before any user creation) ---
	if h.state.GRPCRegistry == nil {
		sendJSONError(w, "gRPC not available", 503)
		return
	}

	// --- Resolve owner ---
	ownerID := ""
	if req.OwnerUserID != nil {
		user, err := h.state.Store.GetUserByID(*req.OwnerUserID)
		if err != nil || user == nil {
			sendJSONError(w, fmt.Sprintf("User with id %s not found", *req.OwnerUserID), 400)
			return
		}
		ownerID = user.ID
	} else {
		// Create a new non-admin user.
		hashed, err := bcrypt.GenerateFromPassword([]byte(req.NewUser.Password), bcrypt.DefaultCost)
		if err != nil {
			sendJSONError(w, "Failed to hash password", 500)
			return
		}
		newUser := &models.User{
			Username: req.NewUser.Username,
			Password: string(hashed),
			IsAdmin:  false,
		}
		if err := h.state.Store.CreateUser(newUser); err != nil {
			log.Printf("AssignOrphan: CreateUser failed for username=%q: %v", req.NewUser.Username, err)
			sendJSONError(w, "Failed to create user (username may already exist)", 409)
			return
		}
		// Region access, like the other two paths that create a user. This one
		// was the exception: CreateUser defaults it to all-regions and the
		// registration flow follows the account policy, while an orphan-assigned
		// account got no row at all - which the region filter reads as "no
		// regions" rather than "not decided". Non-fatal like the sibling in
		// CreateUser: the account exists either way and an admin can fix the
		// assignment, and failing here would leave a user created but unreported.
		if err := h.state.Store.SetUserRegions(newUser.ID, true, []string{}); err != nil {
			log.Printf("AssignOrphan: SetUserRegions failed for userID=%s: %v", newUser.ID, err)
		}
		ownerID = newUser.ID
	}

	// --- Inspect the orphan on the node ---
	orphanInfo, err := inspectOrphanOnNode(h.state, req.NodeID, req.UUID)
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Node communication error: %v", err), 502)
		return
	}

	// The node read this out of a .active_server file on its own disk and it is
	// about to become a DB value that gets filepath.Join'ed onto server data
	// directories - by the node on every install_mod / remove_mod / reconcile,
	// and by Core on the backup paths. Join CLEANS rather than confines, so a
	// name carrying ".." walks out of the server root. Every other entry point
	// for this field already checks it (SwitchSubServer sanitizes, the backup
	// handlers run validate.IsSubServerName with a comment spelling out this
	// exact hazard); adoption was the one that did not, and it is the one whose
	// input comes from a machine the platform does not own - a BYON node.
	activeSubServer := orphanInfo.ActiveSubServer
	if activeSubServer != "" && !validate.IsSubServerName(activeSubServer) {
		log.Printf("AssignOrphan: node %d reported an unusable active sub-server name %q for %s; refusing adoption",
			req.NodeID, activeSubServer, req.UUID)
		sendJSONError(w, "The node reported an invalid active sub-server name for this server", 400)
		return
	}
	installerType, minecraftVersion, buildNumber, _ := subServerLoaderFromInspect(orphanInfo, activeSubServer)

	// --- Create the servers DB row ---
	srv := &models.Server{
		UUID:            req.UUID,
		Name:            strings.TrimSpace(req.Name),
		NodeID:          req.NodeID,
		OwnerID:         ownerID,
		Memory:          req.MemoryMB,
		CPULimit:        req.CPULimit,
		Status:          "stopped",
		ActiveSubServer: activeSubServer,
		ServerType:      "game",
	}

	newID, err := h.state.Store.CreateServer(srv)
	if err != nil {
		log.Printf("AssignOrphan: CreateServer failed (uuid=%s, owner=%s): %v", req.UUID, ownerID, err)
		if req.NewUser != nil {
			log.Printf("AssignOrphan: WARNING — new user (id=%s username=%q) was created but server insert failed; manual cleanup may be required", ownerID, req.NewUser.Username)
		}
		sendJSONError(w, "Failed to create server record", 500)
		return
	}
	srv.ID = int(newID)

	// Persist installer_type, minecraft_version, active_sub_server, build via UpdateServerSetup.
	// game_image and start_command are empty for adopted servers — the node already
	// knows the real command from its own .dylaris.json / active-server file.
	if err := h.state.Store.UpdateServerSetup(srv.ID, "", "", activeSubServer, "", installerType, minecraftVersion, buildNumber); err != nil {
		log.Printf("AssignOrphan: UpdateServerSetup failed (id=%d): %v", srv.ID, err)
		// Non-fatal: the row exists; setup fields just remain empty.
	}

	srv.InstallerType = installerType
	srv.MinecraftVersion = minecraftVersion
	srv.BuildNumber = buildNumber

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"server":  srv,
	})
}
