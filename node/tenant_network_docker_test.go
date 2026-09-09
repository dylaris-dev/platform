package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/docker/docker/api/types/network"
)

// fakeDockerNet is an in-memory dockerNetAPI for daemon-free tests.
type fakeDockerNet struct {
	nets        []network.Summary
	created     []network.CreateOptions
	connects    []string // "netID|container"
	disconnects []string // "netID|container"
	removed     []string
	nextID      int
	// removeErr is returned by NetworkRemove, so a test can stage the failure
	// Docker reports when an endpoint is still attached.
	removeErr error
	// connectErr[container] is returned by NetworkConnect for that container,
	// so a test can stage an absent Link the way Docker reports one.
	connectErr map[string]error
}

func (f *fakeDockerNet) NetworkList(_ context.Context, _ network.ListOptions) ([]network.Summary, error) {
	return f.nets, nil
}
func (f *fakeDockerNet) NetworkInspect(_ context.Context, id string, _ network.InspectOptions) (network.Inspect, error) {
	for _, n := range f.nets {
		if n.ID == id {
			return n, nil
		}
	}
	return network.Inspect{}, fmt.Errorf("no such network: %s", id)
}
func (f *fakeDockerNet) NetworkCreate(_ context.Context, name string, opts network.CreateOptions) (network.CreateResponse, error) {
	f.nextID++
	id := fmt.Sprintf("net%d", f.nextID)
	f.created = append(f.created, opts)
	sum := network.Summary{Name: name, ID: id, Driver: opts.Driver, Labels: opts.Labels}
	if opts.IPAM != nil {
		sum.IPAM = *opts.IPAM
	}
	f.nets = append(f.nets, sum)
	return network.CreateResponse{ID: id}, nil
}
func (f *fakeDockerNet) NetworkConnect(_ context.Context, id, c string, _ *network.EndpointSettings) error {
	if err, ok := f.connectErr[c]; ok {
		return err
	}
	f.connects = append(f.connects, id+"|"+c)
	return nil
}
func (f *fakeDockerNet) NetworkDisconnect(_ context.Context, id, c string, _ bool) error {
	f.disconnects = append(f.disconnects, id+"|"+c)
	return nil
}
func (f *fakeDockerNet) NetworkRemove(_ context.Context, id string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, id)
	return nil
}

func newTestManager(t *testing.T, globalDriver string) (*TenantNetworkManager, *fakeDockerNet) {
	t.Helper()
	f := &fakeDockerNet{nets: []network.Summary{
		{Name: "dylaris_net", ID: "global1", Driver: globalDriver,
			IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: "172.18.0.0/16"}}}},
	}}
	alloc := loadTenantAllocator(t.TempDir())
	return newTenantNetworkManager(f, context.Background(), alloc, "node-host"), f
}

func TestEnsureTenantNetworkCreatesAndIsIdempotent(t *testing.T) {
	m, f := newTestManager(t, "bridge")
	m.mu.Lock()
	name, err := m.EnsureTenantNetwork("owner-A")
	m.mu.Unlock()
	if err != nil {
		t.Fatalf("EnsureTenantNetwork: %v", err)
	}
	if name != "dylaris_tenant_owner-a" {
		t.Fatalf("name = %s, want dylaris_tenant_owner-a", name)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d networks, want 1", len(f.created))
	}
	if got := f.created[0].IPAM.Config[0].Subnet; got != "10.0.0.0/26" {
		t.Fatalf("subnet = %s, want 10.0.0.0/26 (avoids docker 172.18/16)", got)
	}
	if f.created[0].Driver != "bridge" || f.created[0].Attachable {
		t.Fatalf("driver/attachable = %s/%v, want bridge/false", f.created[0].Driver, f.created[0].Attachable)
	}
	// Node AND Link connected to the tenant net. The Link is not optional: a
	// managed server's route target is mc_<uuid> by name, which resolves only
	// on a network the Link is on.
	if len(f.connects) != 2 || f.connects[0] != "net1|node-host" || f.connects[1] != "net1|"+linkContainerName {
		t.Fatalf("connects = %v, want [net1|node-host net1|%s]", f.connects, linkContainerName)
	}
	// Second call: no new network.
	m.mu.Lock()
	_, err = m.EnsureTenantNetwork("owner-A")
	m.mu.Unlock()
	if err != nil {
		t.Fatalf("EnsureTenantNetwork(2): %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("idempotency broken: created %d networks", len(f.created))
	}
}

// AttachLinkToAll is what covers a Link recreated AFTER the tenant nets exist
// (image update, token roll). It must join every tenant net and nothing else -
// putting the Link on dylaris_net a second time is harmless, but joining an
// unrelated network is not.
func TestAttachLinkToAllJoinsOnlyTenantNets(t *testing.T) {
	m, f := newTestManager(t, "bridge")
	f.nets = append(f.nets, network.Summary{Name: "some-other-stack", ID: "foreign1"})
	for _, owner := range []string{"owner-A", "owner-B"} {
		m.mu.Lock()
		if _, err := m.EnsureTenantNetwork(owner); err != nil {
			m.mu.Unlock()
			t.Fatalf("EnsureTenantNetwork(%s): %v", owner, err)
		}
		m.mu.Unlock()
	}
	f.connects = nil // only look at what AttachLinkToAll does

	m.AttachLinkToAll()

	want := []string{"net1|" + linkContainerName, "net2|" + linkContainerName}
	if len(f.connects) != len(want) {
		t.Fatalf("connects = %v, want %v", f.connects, want)
	}
	for i, w := range want {
		if f.connects[i] != w {
			t.Fatalf("connects = %v, want %v", f.connects, want)
		}
	}
}

// A Link that does not exist (direct-port mode, or no gateway configured) is a
// no-op, not a failure: server creation must not depend on a sidecar that this
// deployment never runs.
func TestEnsureTenantNetworkSurvivesAMissingLink(t *testing.T) {
	m, f := newTestManager(t, "bridge")
	f.connectErr = map[string]error{linkContainerName: fmt.Errorf("Error: No such container: %s", linkContainerName)}

	m.mu.Lock()
	name, err := m.EnsureTenantNetwork("owner-A")
	m.mu.Unlock()
	if err != nil {
		t.Fatalf("EnsureTenantNetwork must not fail when the Link is absent: %v", err)
	}
	if name != "dylaris_tenant_owner-a" {
		t.Fatalf("name = %s, want dylaris_tenant_owner-a", name)
	}
}

func TestEnsureTenantNetworkMirrorsOverlayDriver(t *testing.T) {
	m, f := newTestManager(t, "overlay")
	m.mu.Lock()
	_, err := m.EnsureTenantNetwork("owner-A")
	m.mu.Unlock()
	if err != nil {
		t.Fatalf("EnsureTenantNetwork: %v", err)
	}
	if f.created[0].Driver != "overlay" || !f.created[0].Attachable {
		t.Fatalf("driver/attachable = %s/%v, want overlay/true", f.created[0].Driver, f.created[0].Attachable)
	}
}

func TestEndpointsForAssignsFixedIP(t *testing.T) {
	m, _ := newTestManager(t, "bridge")
	nc, err := m.endpointsFor("srv-1", "owner-A")
	if err != nil {
		t.Fatalf("endpointsFor: %v", err)
	}
	ep, ok := nc.EndpointsConfig["dylaris_tenant_owner-a"]
	if !ok {
		t.Fatalf("no endpoint for tenant net in %v", nc.EndpointsConfig)
	}
	if ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address != "10.0.0.4" {
		t.Fatalf("fixed IP = %v, want 10.0.0.4", ep.IPAMConfig)
	}
	// Empty ownerID reverse-resolves via the allocator (restart path).
	nc2, err := m.endpointsFor("srv-1", "")
	if err != nil {
		t.Fatalf("endpointsFor(empty owner): %v", err)
	}
	if _, ok := nc2.EndpointsConfig["dylaris_tenant_owner-a"]; !ok {
		t.Fatalf("empty-owner path did not resolve tenant net: %v", nc2.EndpointsConfig)
	}
}

func TestEndpointsForUnknownServerErrors(t *testing.T) {
	m, _ := newTestManager(t, "bridge")
	if _, err := m.endpointsFor("ghost", ""); err == nil {
		t.Fatal("endpointsFor(unknown, empty owner) err = nil, want error")
	}
}

func TestTenantEndpointsFallbackWhenDisabled(t *testing.T) {
	dm := &DockerManager{} // tenant == nil (isolation off)
	nc := dm.tenantEndpoints("srv-1", "owner-A", "global-net-id", "dylaris_net")
	ep, ok := nc.EndpointsConfig["dylaris_net"]
	if !ok || ep.NetworkID != "global-net-id" {
		t.Fatalf("fallback endpoints = %v, want dylaris_net -> global-net-id", nc.EndpointsConfig)
	}
}

// TestTenantEndpointsUsesTheResolvedNetworkName: the endpoint has to be keyed by
// the name Docker knows the network by, which on a compose or stack deployment
// carries a project prefix. Keying the literal "dylaris_net" there names no
// network, and Docker then creates the container with NO endpoint at all rather
// than failing - a server that is Up and can reach nothing.
func TestTenantEndpointsUsesTheResolvedNetworkName(t *testing.T) {
	dm := &DockerManager{}
	nc := dm.tenantEndpoints("srv-1", "owner-A", "global-net-id", "platform_dylaris_net")
	if _, wrong := nc.EndpointsConfig["dylaris_net"]; wrong {
		t.Error("endpoint keyed by the bare name; on this host no such network exists")
	}
	ep, ok := nc.EndpointsConfig["platform_dylaris_net"]
	if !ok || ep.NetworkID != "global-net-id" {
		t.Fatalf("endpoints = %v, want platform_dylaris_net -> global-net-id", nc.EndpointsConfig)
	}
}

// TestTenantEndpointsFallsBackToTheBareNameWhenUnresolved keeps the old
// behaviour for a caller that has no resolved name to give.
func TestTenantEndpointsFallsBackToTheBareNameWhenUnresolved(t *testing.T) {
	dm := &DockerManager{}
	nc := dm.tenantEndpoints("srv-1", "owner-A", "global-net-id", "")
	if _, ok := nc.EndpointsConfig["dylaris_net"]; !ok {
		t.Fatalf("endpoints = %v, want the bare dylaris_net key", nc.EndpointsConfig)
	}
}

func TestReleaseRemovesNetworkOnLastServer(t *testing.T) {
	m, f := newTestManager(t, "bridge")
	// Two servers for owner-A; create the network.
	if _, err := m.endpointsFor("srv-1", "owner-A"); err != nil {
		t.Fatalf("endpointsFor srv-1: %v", err)
	}
	if _, err := m.endpointsFor("srv-2", "owner-A"); err != nil {
		t.Fatalf("endpointsFor srv-2: %v", err)
	}
	// The tenant net exists (net1). Release one: net stays.
	m.release("srv-1")
	for _, id := range f.removed {
		if id == "net1" {
			t.Fatalf("net removed while srv-2 still present")
		}
	}
	// Release the last: net is removed.
	m.release("srv-2")
	removedNet1 := false
	for _, id := range f.removed {
		if id == "net1" {
			removedNet1 = true
		}
	}
	if !removedNet1 {
		t.Fatalf("tenant net not removed after last server release; removed=%v", f.removed)
	}
	if _, ok := m.alloc.subnetString("owner-A"); ok {
		t.Fatalf("owner-A subnet not freed after last release")
	}
}

func TestServerConfigDecodesOwnerID(t *testing.T) {
	// Exactly the shape Core's create payload marshals (map[string]interface{}).
	payload := []byte(`{"uuid":"srv-1","ownerId":"owner-A","activeSubServer":"server",` +
		`"docker":{"image":"img","ram":2048,"cpuLimit":2}}`)
	var cfg ServerConfig
	if err := json.Unmarshal(payload, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.OwnerID != "owner-A" {
		t.Fatalf("OwnerID = %q, want owner-A", cfg.OwnerID)
	}
	if cfg.UUID != "srv-1" || cfg.Docker.RAM != 2048 {
		t.Fatalf("other fields lost: %+v", cfg)
	}
}

// Tearing a tenant network down has to detach the LINK as well as the node.
//
// The Link is attached to every tenant network (connectLink) on any deployment
// that carries player traffic, and Docker refuses to remove a network that
// still has an endpoint. release() knew this; the enlarge path had its own copy
// of the teardown and that copy disconnected only the node - so its remove
// could not succeed in exactly the deployments it is asked to run in. There is
// one teardown now, and this pins both endpoints.
func TestRemoveTenantNetworkDetachesTheLinkToo(t *testing.T) {
	m, f := newTestManager(t, "bridge")

	name, err := m.EnsureTenantNetwork("owner-A")
	if err != nil {
		t.Fatalf("EnsureTenantNetwork: %v", err)
	}
	id, found, _ := m.findNetwork(name)
	if !found {
		t.Fatal("the network under test does not exist")
	}
	f.disconnects = nil

	removed, err := m.removeTenantNetwork(name)
	if err != nil || !removed {
		t.Fatalf("removeTenantNetwork = (%v, %v), want (true, nil)", removed, err)
	}

	want := map[string]bool{id + "|node-host": false, id + "|" + linkContainerName: false}
	for _, d := range f.disconnects {
		if _, ok := want[d]; ok {
			want[d] = true
		}
	}
	for d, seen := range want {
		if !seen {
			t.Errorf("%s was never disconnected; Docker then refuses the remove with \"has active endpoints\"", d)
		}
	}
	if len(f.removed) != 1 || f.removed[0] != id {
		t.Errorf("removed = %v, want [%s]", f.removed, id)
	}
}

// A network that is not there is not an error, and is not a removal either.
// release() logs "removed empty tenant net" off this, and saying it about a
// network nobody found would be a line an operator cannot act on.
func TestRemoveTenantNetworkOnAMissingNetwork(t *testing.T) {
	m, _ := newTestManager(t, "bridge")

	removed, err := m.removeTenantNetwork(tenantNetworkName("owner-nobody"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if removed {
		t.Error("reported a removal of a network that did not exist")
	}
}

// A remove that fails must SAY so rather than reporting success. Everything the
// enlarge path does afterwards - creating the network at the new subnet,
// connecting the node at an address inside it - is wrong if the old network is
// still standing under the same name.
func TestRemoveTenantNetworkReportsAFailedRemove(t *testing.T) {
	m, f := newTestManager(t, "bridge")

	name, err := m.EnsureTenantNetwork("owner-A")
	if err != nil {
		t.Fatalf("EnsureTenantNetwork: %v", err)
	}
	f.removeErr = fmt.Errorf("network %s has active endpoints", name)

	removed, err := m.removeTenantNetwork(name)
	if err == nil {
		t.Fatal("a failed remove reported success")
	}
	if removed {
		t.Error("reported a removal that did not happen")
	}
}
