package services

import (
	"testing"

	"dylaris-core/models"
)

// tagFakeStore records the tag writes on top of the region fake, so one test
// can watch both fields the node supplies through its environment.
type tagFakeStore struct {
	*regionFakeStore
	tags []string
}

func (f *tagFakeStore) SetNodeTags(id int, tags string) error {
	f.tags = append(f.tags, tags)
	return nil
}

func newEnvDiscovery(known ...string) (*DiscoveryService, *tagFakeStore) {
	svc, region := newRegionDiscovery(known...)
	f := &tagFakeStore{regionFakeStore: region}
	svc.store = f
	return svc, f
}

// A node that was adopted through the old Configure dialog carries
// configured=true forever - nothing clears it. Its environment must still win,
// otherwise the field an operator edits on the node is the one field that can
// never take effect, and there is no longer any panel control to correct it.
func TestHeartbeatEnvWinsOverAnAdoptedNode(t *testing.T) {
	svc, f := newEnvDiscovery("eu-central")
	node := &models.Node{ID: 1, Name: "node-a", Configured: true, Tags: "set-in-panel", Region: "eu-west"}

	svc.syncNodeMetadataFromHeartbeat(node, NodeHeartbeat{Tags: "from-env", Region: "eu-central"})

	if len(f.tags) != 1 || f.tags[0] != "from-env" {
		t.Errorf("tags written = %v, want [from-env]", f.tags)
	}
	if len(f.written) != 1 || f.written[0] != "eu-central" {
		t.Errorf("regions written = %v, want [eu-central]", f.written)
	}
}

// An empty field is the node saying nothing, not saying "none". A node that
// never sets NODE_REGION would otherwise have its region blanked on every beat,
// five seconds apart, and placement would lose it.
func TestASilentHeartbeatLeavesTheRowAlone(t *testing.T) {
	svc, f := newEnvDiscovery("eu-central")
	node := &models.Node{ID: 1, Name: "node-a", Tags: "keep-me", Region: "eu-central"}

	svc.syncNodeMetadataFromHeartbeat(node, NodeHeartbeat{})

	if len(f.tags) != 0 {
		t.Errorf("tags written = %v, want none", f.tags)
	}
	if len(f.written) != 0 {
		t.Errorf("regions written = %v, want none", f.written)
	}
}

// Nothing is written when the environment already agrees with the row, so the
// five-second scan does not turn into an UPDATE per node per beat.
func TestAnAgreeingHeartbeatWritesNothing(t *testing.T) {
	svc, f := newEnvDiscovery("eu-central")
	node := &models.Node{ID: 1, Name: "node-a", Tags: "same", Region: "eu-central"}

	svc.syncNodeMetadataFromHeartbeat(node, NodeHeartbeat{Tags: "same", Region: "eu-central"})

	if len(f.tags) != 0 {
		t.Errorf("tags written = %v, want none", f.tags)
	}
	if len(f.written) != 0 {
		t.Errorf("regions written = %v, want none - the region already matches", f.written)
	}
}
