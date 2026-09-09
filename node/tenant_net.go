package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const (
	// tenantPrefixDefault is the block size handed to a new owner: /26 = 64
	// addresses, ~59 usable server slots after network/gateway/node/link/broadcast.
	tenantPrefixDefault = 26
	// tenantSlotBase reserves .0 network, .1 gateway (Docker default), .2 the
	// node and .3 the Link sidecar. Server slots are 1-based and start at .4
	// (base + tenantSlotBase + 1 + slot).
	//
	// The Link's address is PINNED for the same reason the node's is: connecting
	// it without one let Docker's dynamic IPAM hand it .3, which the allocator
	// had reserved for server slot 1 - the server then failed to start with
	// "Address already in use" and the reconciler retried forever. Measured
	// 2026-08-24; the collision is invisible until a pinned server restarts.
	tenantSlotBase = 2
)

// tenantPools are the private ranges the allocator draws blocks from, in order.
// 10/8 first (largest); 172.16/12 and 192.168/16 are fallbacks per the design.
func tenantPools() []*net.IPNet {
	out := make([]*net.IPNet, 0, 3)
	for _, c := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		_, n, _ := net.ParseCIDR(c) // literals: err is always nil
		out = append(out, n)
	}
	return out
}

// cidrsOverlap reports whether two IPv4 CIDR ranges share any address.
func cidrsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

// parseCIDRs parses CIDR strings, skipping empties and non-IPv4/unparseable
// entries (a Docker network without an IPAM subnet yields "").
func parseCIDRs(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil || n.IP.To4() == nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// capacityForPrefix returns the usable server slots in a subnet of the given
// prefix length: total addresses minus network, gateway, node, Link and
// broadcast.
func capacityForPrefix(prefixLen int) int {
	usable := (1 << (32 - prefixLen)) - 5
	if usable < 0 {
		return 0
	}
	return usable
}

// nextPrefixLen returns the next-larger block size for an enlarge (/26 -> /25 ->
// /24). ok=false when the block cannot grow further (/24 is the widest allowed).
func nextPrefixLen(current int) (int, bool) {
	if current <= 24 {
		return 0, false
	}
	return current - 1, true
}

// nodeIPInSubnet returns the fixed IPv4 the node pins for itself: network + 2.
func nodeIPInSubnet(subnet *net.IPNet) net.IP {
	return offsetIP(subnet, uint32(tenantSlotBase))
}

// linkIPInSubnet returns the fixed IPv4 the Link sidecar pins: network + 3,
// immediately after the node and before the first server slot.
func linkIPInSubnet(subnet *net.IPNet) net.IP {
	return offsetIP(subnet, uint32(tenantSlotBase)+1)
}

// serverIPInSubnet returns the fixed IPv4 for a 1-based server slot: network +
// tenantSlotBase + 1 + slot (slot 1 = .4 when the subnet base is .0).
func serverIPInSubnet(subnet *net.IPNet, slot int) net.IP {
	return offsetIP(subnet, uint32(tenantSlotBase)+1+uint32(slot))
}

func offsetIP(subnet *net.IPNet, off uint32) net.IP {
	base := binary.BigEndian.Uint32(subnet.IP.To4())
	out := make(net.IP, 4)
	binary.BigEndian.PutUint32(out, base+off)
	return out
}

// nextFreeSubnet scans tenantPools in order and returns the first subnet of the
// given prefix length not overlapping any subnet in used. Errors when exhausted.
func nextFreeSubnet(used []*net.IPNet, prefixLen int) (*net.IPNet, error) {
	step := uint32(1) << (32 - prefixLen)
	mask := net.CIDRMask(prefixLen, 32)
	for _, pool := range tenantPools() {
		poolStart := binary.BigEndian.Uint32(pool.IP.To4())
		poolOnes, _ := pool.Mask.Size()
		poolEnd := poolStart + (uint32(1) << (32 - poolOnes)) // exclusive
		for base := poolStart; base >= poolStart && base+step <= poolEnd; base += step {
			ipBytes := make(net.IP, 4)
			binary.BigEndian.PutUint32(ipBytes, base)
			cand := &net.IPNet{IP: ipBytes, Mask: mask}
			overlap := false
			for _, u := range used {
				if cidrsOverlap(cand, u) {
					overlap = true
					break
				}
			}
			if !overlap {
				return cand, nil
			}
		}
	}
	return nil, fmt.Errorf("tenant allocator: no free /%d block in private pools", prefixLen)
}

// errSubnetFull signals a tenant subnet has no free slot; callers enlarge.
var errSubnetFull = errors.New("tenant allocator: subnet full")

// ownerAlloc is one owner's assignment: its subnet, the serverUUID->slot map,
// and the next slot to hand out. NextSlot is monotonic (freed slots are NOT
// reused) so a server's fixed IP is stable across the owner's lifetime.
type ownerAlloc struct {
	Subnet   string         `json:"subnet"`
	Slots    map[string]int `json:"slots"`
	NextSlot int            `json:"nextSlot"`
}

type tenantState struct {
	Owners map[string]*ownerAlloc `json:"owners"`
}

// TenantAllocator maps owners to /26 subnets and servers to fixed IPs, persisted
// to disk beside .node_secret so assignments survive node restarts. NOT safe for
// concurrent use; the TenantNetworkManager serializes access under its mutex.
type TenantAllocator struct {
	path  string
	state *tenantState
}

// tenantStatePath is the on-disk allocator file, beside .node_secret.
func tenantStatePath(dir string) string {
	return filepath.Join(dir, ".tenant_networks.json")
}

// loadTenantAllocator reads persisted state; a missing or malformed file yields
// an empty allocator (same resilience posture as loadNodeSecret).
func loadTenantAllocator(dir string) *TenantAllocator {
	a := &TenantAllocator{
		path:  tenantStatePath(dir),
		state: &tenantState{Owners: map[string]*ownerAlloc{}},
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		return a
	}
	var st tenantState
	if err := json.Unmarshal(data, &st); err != nil || st.Owners == nil {
		return a
	}
	a.state = &st
	return a
}

// save writes the state as JSON with 0600 perms (like saveNodeSecret).
func (a *TenantAllocator) save() error {
	data, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.path, data, 0600)
}

// subnetString returns an owner's subnet CIDR (ok=false when unknown).
func (a *TenantAllocator) subnetString(ownerID string) (string, bool) {
	if o, ok := a.state.Owners[ownerID]; ok {
		return o.Subnet, true
	}
	return "", false
}

// usedSubnets unions externally-supplied Docker subnets with every owner's
// currently-allocated subnet, so a new pick overlaps neither.
func (a *TenantAllocator) usedSubnets(extra []*net.IPNet) []*net.IPNet {
	used := append([]*net.IPNet{}, extra...)
	for _, o := range a.state.Owners {
		if _, n, err := net.ParseCIDR(o.Subnet); err == nil {
			used = append(used, n)
		}
	}
	return used
}

// ensureSubnet returns the owner's subnet, allocating a new /26 (avoiding
// usedDockerSubnets + all allocated subnets) when the owner is unknown.
func (a *TenantAllocator) ensureSubnet(ownerID string, usedDockerSubnets []*net.IPNet) (*net.IPNet, error) {
	if o, ok := a.state.Owners[ownerID]; ok {
		_, n, err := net.ParseCIDR(o.Subnet)
		if err != nil {
			return nil, fmt.Errorf("corrupt subnet %q for owner %s: %w", o.Subnet, ownerID, err)
		}
		return n, nil
	}
	free, err := nextFreeSubnet(a.usedSubnets(usedDockerSubnets), tenantPrefixDefault)
	if err != nil {
		return nil, err
	}
	a.state.Owners[ownerID] = &ownerAlloc{
		Subnet:   free.String(),
		Slots:    map[string]int{},
		NextSlot: 1,
	}
	return free, a.save()
}

// allocateSlot returns the stable slot for a server, assigning the next free one
// when new. errSubnetFull when the subnet is exhausted (caller enlarges).
func (a *TenantAllocator) allocateSlot(ownerID, serverUUID string) (int, error) {
	o, ok := a.state.Owners[ownerID]
	if !ok {
		return 0, fmt.Errorf("tenant allocator: owner %s has no subnet", ownerID)
	}
	if slot, ok := o.Slots[serverUUID]; ok {
		return slot, nil
	}
	_, n, err := net.ParseCIDR(o.Subnet)
	if err != nil {
		return 0, err
	}
	ones, _ := n.Mask.Size()
	if o.NextSlot > capacityForPrefix(ones) {
		return 0, errSubnetFull
	}
	slot := o.NextSlot
	o.Slots[serverUUID] = slot
	o.NextSlot++
	return slot, a.save()
}

// ipFor computes the fixed IP for a server from its subnet + slot.
func (a *TenantAllocator) ipFor(ownerID, serverUUID string) (net.IP, *net.IPNet, error) {
	o, ok := a.state.Owners[ownerID]
	if !ok {
		return nil, nil, fmt.Errorf("tenant allocator: owner %s has no subnet", ownerID)
	}
	slot, ok := o.Slots[serverUUID]
	if !ok {
		return nil, nil, fmt.Errorf("tenant allocator: server %s has no slot for owner %s", serverUUID, ownerID)
	}
	_, n, err := net.ParseCIDR(o.Subnet)
	if err != nil {
		return nil, nil, err
	}
	return serverIPInSubnet(n, slot), n, nil
}

// ownerForServer reverse-looks-up the owner holding a serverUUID slot. Used by
// the restart/reconcile paths where the command config carries no OwnerID.
func (a *TenantAllocator) ownerForServer(serverUUID string) (string, bool) {
	for owner, o := range a.state.Owners {
		if _, ok := o.Slots[serverUUID]; ok {
			return owner, true
		}
	}
	return "", false
}

// serversForOwner lists an owner's server UUIDs (for enlarge rejoins).
func (a *TenantAllocator) serversForOwner(ownerID string) []string {
	o, ok := a.state.Owners[ownerID]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(o.Slots))
	for u := range o.Slots {
		out = append(out, u)
	}
	return out
}

// release drops a server's slot; returns true when the owner has no servers
// left (caller then removes the network + frees the subnet).
func (a *TenantAllocator) release(ownerID, serverUUID string) (bool, error) {
	o, ok := a.state.Owners[ownerID]
	if !ok {
		return false, nil
	}
	delete(o.Slots, serverUUID)
	if len(o.Slots) == 0 {
		delete(a.state.Owners, ownerID)
		return true, a.save()
	}
	return false, a.save()
}

// enlarge moves an owner to a larger block (/26->/25->/24). Slots (serverUUID->
// index) are preserved so IPs remap deterministically and mc_<uuid> hostnames
// stay stable. usedDockerSubnets includes the owner's live (old) network, so the
// new block is a fresh, non-overlapping region.
func (a *TenantAllocator) enlarge(ownerID string, usedDockerSubnets []*net.IPNet) (oldNet, newNet *net.IPNet, err error) {
	o, ok := a.state.Owners[ownerID]
	if !ok {
		return nil, nil, fmt.Errorf("tenant allocator: owner %s has no subnet", ownerID)
	}
	_, oldNet, err = net.ParseCIDR(o.Subnet)
	if err != nil {
		return nil, nil, err
	}
	ones, _ := oldNet.Mask.Size()
	nextLen, ok := nextPrefixLen(ones)
	if !ok {
		return nil, nil, fmt.Errorf("tenant allocator: owner %s already at max block /%d", ownerID, ones)
	}
	// Avoid docker subnets + every OTHER owner's subnet (self is excluded from
	// the owner loop but is present in usedDockerSubnets as a live network).
	var used []*net.IPNet
	for id, oa := range a.state.Owners {
		if id == ownerID {
			continue
		}
		if _, n, e := net.ParseCIDR(oa.Subnet); e == nil {
			used = append(used, n)
		}
	}
	used = append(used, usedDockerSubnets...)
	newNet, err = nextFreeSubnet(used, nextLen)
	if err != nil {
		return nil, nil, err
	}
	o.Subnet = newNet.String()
	return oldNet, newNet, a.save()
}

// restoreSubnet puts an owner back on the block enlarge() moved them off.
//
// enlarge writes the new subnet to disk before any Docker work is attempted,
// which is the only order that keeps two callers from picking the same free
// block. The cost is that an abandoned enlarge leaves the file describing a
// network nobody built: every address handed out afterwards sits outside the
// live network, and nothing on it starts. This is how that is undone.
func (a *TenantAllocator) restoreSubnet(ownerID string, subnet *net.IPNet) error {
	if subnet == nil {
		return nil
	}
	o, ok := a.state.Owners[ownerID]
	if !ok {
		return nil
	}
	if o.Subnet == subnet.String() {
		return nil
	}
	o.Subnet = subnet.String()
	return a.save()
}
