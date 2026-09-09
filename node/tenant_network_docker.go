package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/network"
)

// dockerNetAPI is the minimal Docker network surface the tenant layer needs, so
// pool discovery + EnsureTenantNetwork are unit-testable with a fake (no
// daemon). *client.Client satisfies it (signatures verified against SDK v28.5.2).
type dockerNetAPI interface {
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
	NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error)
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error
	NetworkRemove(ctx context.Context, networkID string) error
}

const globalNetworkName = "dylaris_net"

// tenantNetworkName is the Docker network name for an owner's tenant net.
func tenantNetworkName(ownerID string) string {
	return "dylaris_tenant_" + strings.ToLower(ownerID)
}

// TenantNetworkManager owns the per-owner network lifecycle: pool discovery,
// allocation (via TenantAllocator) and Docker network create/connect/remove. A
// single mutex serializes allocator mutations + network ops.
type TenantNetworkManager struct {
	api           dockerNetAPI
	ctx           context.Context
	alloc         *TenantAllocator
	nodeContainer string // this node's own container name (os.Hostname())
	mu            sync.Mutex
}

func newTenantNetworkManager(api dockerNetAPI, ctx context.Context, alloc *TenantAllocator, nodeContainer string) *TenantNetworkManager {
	return &TenantNetworkManager{api: api, ctx: ctx, alloc: alloc, nodeContainer: nodeContainer}
}

// discoverUsedSubnets enumerates every Docker network's IPAM subnets so the
// allocator can pick a non-overlapping block. No env override: this IS the pool.
func (t *TenantNetworkManager) discoverUsedSubnets() ([]*net.IPNet, error) {
	nets, err := t.api.NetworkList(t.ctx, network.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("network list: %w", err)
	}
	var cidrs []string
	for _, n := range nets {
		for _, c := range n.IPAM.Config {
			if c.Subnet != "" {
				cidrs = append(cidrs, c.Subnet)
			}
		}
	}
	return parseCIDRs(cidrs), nil
}

// mirroredDriver inspects dylaris_net and returns its driver (bridge single-host,
// overlay on Swarm) and whether the tenant net must be Attachable (overlay only).
func (t *TenantNetworkManager) mirroredDriver() (string, bool) {
	nets, err := t.api.NetworkList(t.ctx, network.ListOptions{})
	if err == nil {
		for _, n := range nets {
			if n.Name == globalNetworkName || strings.HasSuffix(n.Name, "_"+globalNetworkName) {
				if insp, ierr := t.api.NetworkInspect(t.ctx, n.ID, network.InspectOptions{}); ierr == nil && insp.Driver != "" {
					return insp.Driver, insp.Driver == "overlay"
				}
				if n.Driver != "" {
					return n.Driver, n.Driver == "overlay"
				}
			}
		}
	}
	return "bridge", false // safe single-host default
}

// findNetwork returns a network's ID by exact name.
func (t *TenantNetworkManager) findNetwork(name string) (string, bool, error) {
	nets, err := t.api.NetworkList(t.ctx, network.ListOptions{})
	if err != nil {
		return "", false, fmt.Errorf("network list: %w", err)
	}
	for _, n := range nets {
		if n.Name == name {
			return n.ID, true, nil
		}
	}
	return "", false, nil
}

// EnsureTenantNetwork creates the owner's network if missing (subnet from the
// allocator, driver mirroring dylaris_net), connects this node, and returns the
// network name. Idempotent. CALLER MUST HOLD t.mu.
func (t *TenantNetworkManager) EnsureTenantNetwork(ownerID string) (string, error) {
	name := tenantNetworkName(ownerID)

	used, err := t.discoverUsedSubnets()
	if err != nil {
		return "", err
	}
	subnet, err := t.alloc.ensureSubnet(ownerID, used)
	if err != nil {
		return "", err
	}

	netID, found, err := t.findNetwork(name)
	if err != nil {
		return "", err
	}
	if !found {
		driver, attachable := t.mirroredDriver()
		res, cErr := t.api.NetworkCreate(t.ctx, name, network.CreateOptions{
			Driver:     driver,
			Attachable: attachable,
			IPAM:       &network.IPAM{Config: []network.IPAMConfig{{Subnet: subnet.String()}}},
			Labels: map[string]string{
				"dylaris.role":  "tenant-network",
				"dylaris.owner": ownerID,
			},
		})
		if cErr != nil {
			return "", fmt.Errorf("tenant network create %s: %w", name, cErr)
		}
		netID = res.ID
		log.Printf("tenant-net: created %s (subnet %s driver %s)", name, subnet, driver)
	}

	if err := t.connectNode(netID, nodeIPInSubnet(subnet)); err != nil {
		return "", err
	}
	t.connectLink(netID, linkIPInSubnet(subnet))
	return name, nil
}

// connectNode pins the node into the tenant net so mc_<uuid> DNS + RCON/stats/
// tab-proxy keep working. Idempotent (already-connected is success).
func (t *TenantNetworkManager) connectNode(netID string, ip net.IP) error {
	err := t.api.NetworkConnect(t.ctx, netID, t.nodeContainer, &network.EndpointSettings{
		IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: ip.String()},
	})
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "already connected") {
		return nil
	}
	return fmt.Errorf("connect node to tenant net: %w", err)
}

// connectLink attaches the node-managed Link sidecar to a tenant net.
//
// A managed server's route target is its CONTAINER NAME (mc_<uuid>, written by
// Core's hub bridge), so it resolves only on a network the Link is on. Without
// this an isolated server has no player ingress at all - and nothing looks
// wrong: the route exists, the container runs, the Link is healthy, and the
// name simply never resolves. Measured 2026-08-24.
//
// The address is PINNED (linkIPInSubnet), not left to Docker: a dynamic address
// comes out of the same space the allocator pins server slots in, and Docker
// hands out the lowest free one - which is a stopped server's reserved address.
//
// No Link container (direct-port mode, or no gateway configured) is a no-op,
// not a failure.
func (t *TenantNetworkManager) connectLink(netID string, ip net.IP) {
	err := t.api.NetworkConnect(t.ctx, netID, linkContainerName, &network.EndpointSettings{
		IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: ip.String()},
	})
	if err == nil {
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "already exists") || strings.Contains(msg, "already connected") || strings.Contains(msg, "No such container") {
		return
	}
	log.Printf("tenant-net: could not attach %s to %s - isolated servers on it have no player ingress: %v", linkContainerName, netID, err)
}

// AttachLinkToAll connects the Link sidecar to every existing tenant net. The
// Link is recreated on its own schedule (image update, token roll), so one that
// comes up after the tenant nets exist would otherwise never join them.
func (t *TenantNetworkManager) AttachLinkToAll() {
	t.mu.Lock()
	defer t.mu.Unlock()

	nets, err := t.api.NetworkList(t.ctx, network.ListOptions{})
	if err != nil {
		log.Printf("tenant-net: cannot list networks to attach %s: %v", linkContainerName, err)
		return
	}
	for _, n := range nets {
		if n.Labels["dylaris.role"] != "tenant-network" {
			continue
		}
		// The pinned address is derived from the network's OWN subnet, so an
		// enlarged tenant net gets the right one without any stored state.
		for _, c := range n.IPAM.Config {
			_, subnet, err := net.ParseCIDR(c.Subnet)
			if err != nil {
				continue
			}
			t.connectLink(n.ID, linkIPInSubnet(subnet))
			break
		}
	}
}

// resolveOwner returns the owner for a server: the supplied ownerID when set,
// else the allocator's reverse lookup (restart/reconcile paths).
func (t *TenantNetworkManager) resolveOwner(serverUUID, ownerID string) (string, bool) {
	if ownerID != "" {
		return ownerID, true
	}
	return t.alloc.ownerForServer(serverUUID)
}

// endpointsFor resolves the owner + fixed IP for a server, ensures the tenant
// network exists, and returns the container attach config. Returns errSubnetFull
// (wrapped) when the owner's subnet is exhausted so the caller can enlarge.
func (t *TenantNetworkManager) endpointsFor(serverUUID, ownerID string) (*network.NetworkingConfig, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	owner, ok := t.resolveOwner(serverUUID, ownerID)
	if !ok {
		return nil, fmt.Errorf("no owner known for server %s and none supplied", serverUUID)
	}

	name, err := t.EnsureTenantNetwork(owner)
	if err != nil {
		return nil, err
	}
	if _, err := t.alloc.allocateSlot(owner, serverUUID); err != nil {
		return nil, err // may be errSubnetFull (wrapped by fmt below)
	}
	ip, _, err := t.alloc.ipFor(owner, serverUUID)
	if err != nil {
		return nil, err
	}
	return &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			name: {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: ip.String()}},
		},
	}, nil
}

// release drops a server's slot and, when it was the owner's last, disconnects
// the node and removes the now-empty tenant network + frees the subnet.
func (t *TenantNetworkManager) release(serverUUID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	owner, ok := t.alloc.ownerForServer(serverUUID)
	if !ok {
		return
	}
	empty, err := t.alloc.release(owner, serverUUID)
	if err != nil {
		log.Printf("tenant-net: release %s: %v", serverUUID, err)
	}
	if !empty {
		return
	}
	name := tenantNetworkName(owner)
	switch removed, rerr := t.removeTenantNetwork(name); {
	case rerr != nil:
		log.Printf("tenant-net: remove empty %s: %v", name, rerr)
	case removed:
		log.Printf("tenant-net: removed empty tenant net %s", name)
	}
}

// removeTenantNetwork detaches everything this node put on an owner's network
// and removes it. Reports whether a network was actually there to remove.
// CALLER MUST HOLD t.mu.
//
// BOTH endpoints, always, and that is the whole reason this is one function
// instead of the two copies it replaces. The node is the obvious one. The Link
// is attached to every tenant network as well (connectLink), on any deployment
// that carries player traffic - and the enlarge path disconnected only the
// node, so its remove could not succeed while a Link was running, which is
// exactly when it is asked to.
func (t *TenantNetworkManager) removeTenantNetwork(name string) (bool, error) {
	id, found, err := t.findNetwork(name)
	if err != nil || !found {
		return false, err
	}
	_ = t.api.NetworkDisconnect(t.ctx, id, t.nodeContainer, true)
	_ = t.api.NetworkDisconnect(t.ctx, id, linkContainerName, true)
	if rerr := t.api.NetworkRemove(t.ctx, id); rerr != nil {
		return false, rerr
	}
	return true, nil
}
