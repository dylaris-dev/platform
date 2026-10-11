package services

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// regionFakeStore embeds store.Store (nil) so it satisfies the full interface
// while implementing only the calls this path makes; anything else panics
// loudly rather than passing quietly.
type regionFakeStore struct {
	store.Store
	known     map[string]bool
	written   []string
	ensured   []string
	ensureErr error
	admins    map[string]bool
	lookups   int
}

func (f *regionFakeStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, IsAdmin: f.admins[id]}, nil
}

func (f *regionFakeStore) EnsureRegion(id string) error {
	if f.ensureErr != nil {
		return f.ensureErr
	}
	f.ensured = append(f.ensured, id)
	f.known[id] = true
	return nil
}

func (f *regionFakeStore) GetRegion(id string) (*models.Region, error) {
	f.lookups++
	if f.known[id] {
		return &models.Region{ID: id}, nil
	}
	return nil, errors.New("no such region")
}

func (f *regionFakeStore) SetNodeRegion(id int, region string) error {
	f.written = append(f.written, region)
	return nil
}

func newRegionDiscovery(known ...string) (*DiscoveryService, *regionFakeStore) {
	f := &regionFakeStore{known: map[string]bool{}}
	for _, k := range known {
		f.known[k] = true
	}
	return &DiscoveryService{store: f, badRegionReported: map[int]string{}, regionKnown: map[string]bool{}}, f
}

// captureLog redirects the standard logger for one test. logErrf writes there
// as well as to the error stream, and the stream is nil in tests, so this is
// where a report is observable.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(flags)
	})
	return &buf
}

// Regions are announced by the machines in them, not created in the panel: a
// valid id nobody has seen before gets its row and is then applied. Before,
// it was ignored and the node stayed in "default" until an admin created it.
func TestANewValidHeartbeatRegionIsCreatedAndApplied(t *testing.T) {
	d, f := newRegionDiscovery("eu")
	captureLog(t)

	d.applyHeartbeatRegion(&models.Node{ID: 1, Name: "n1", Region: "default"}, "us-east")

	if len(f.ensured) != 1 || f.ensured[0] != "us-east" {
		t.Fatalf("ensured = %v, want exactly [us-east]", f.ensured)
	}
	if len(f.written) != 1 || f.written[0] != "us-east" {
		t.Fatalf("written = %v, want exactly [us-east]", f.written)
	}
}

func TestAnExistingRegionIsNotRecreated(t *testing.T) {
	d, f := newRegionDiscovery("eu")
	captureLog(t)

	d.applyHeartbeatRegion(&models.Node{ID: 1, Name: "n1", Region: "default"}, "eu")

	if len(f.ensured) != 0 {
		t.Fatalf("an existing region was created again: %v", f.ensured)
	}
}

// nodes.region is a join key and a BYON node is a customer machine that sets
// NODE_REGION freely, so anything that is not a short lowercase id must not
// reach the column or create a row. 32 is the width of regions.id.
func TestAnInvalidHeartbeatRegionIsNotWritten(t *testing.T) {
	for _, bad := range []string{"eu_central", "eu central", "eu/../x", "<script>", "ëu", strings.Repeat("a", 33)} {
		t.Run(bad, func(t *testing.T) {
			d, f := newRegionDiscovery("eu")
			captureLog(t)

			d.applyHeartbeatRegion(&models.Node{ID: 1, Name: "n1", Region: "default"}, bad)

			if len(f.written) != 0 || len(f.ensured) != 0 {
				t.Fatalf("an invalid region got through: written=%v ensured=%v", f.written, f.ensured)
			}
		})
	}
	d, f := newRegionDiscovery()
	captureLog(t)
	d.applyHeartbeatRegion(&models.Node{ID: 1, Name: "n1", Region: "default"}, strings.Repeat("a", 32))
	if len(f.written) != 1 {
		t.Fatalf("a 32-char id (the column width) was refused: %v", f.written)
	}
}

// The column must never name a region no row describes, so a failed insert
// leaves the node where it is and the next heartbeat tries again.
func TestARegionThatCouldNotBeCreatedIsNotWritten(t *testing.T) {
	d, f := newRegionDiscovery("eu")
	f.ensureErr = errors.New("db down")
	captureLog(t)

	d.applyHeartbeatRegion(&models.Node{ID: 1, Name: "n1", Region: "default"}, "us-east")

	if len(f.written) != 0 {
		t.Fatalf("written = %v after the region insert failed", f.written)
	}
}

// Refusing over casing alone would be its own bug: the operator named a region
// that plainly exists. Region ids are canonically lowercase, so the reported
// value is normalised to that form rather than rejected.
func TestAKnownRegionIsNormalisedRatherThanRefused(t *testing.T) {
	d, f := newRegionDiscovery("eu")
	captureLog(t)

	d.applyHeartbeatRegion(&models.Node{ID: 1, Name: "n1", Region: "default"}, "  EU  ")

	if len(f.written) != 1 || f.written[0] != "eu" {
		t.Fatalf("written = %v, want exactly [eu]", f.written)
	}
}

func TestARegionThatAlreadyMatchesIsNotRewritten(t *testing.T) {
	d, f := newRegionDiscovery("eu")
	captureLog(t)

	d.applyHeartbeatRegion(&models.Node{ID: 1, Name: "n1", Region: "eu"}, "EU")

	if len(f.written) != 0 {
		t.Fatalf("rewrote a region that was already correct: %v", f.written)
	}
}

func TestAnEmptyHeartbeatRegionIsIgnored(t *testing.T) {
	d, f := newRegionDiscovery("eu")
	captureLog(t)

	d.applyHeartbeatRegion(&models.Node{ID: 1, Name: "n1", Region: "eu"}, "   ")

	if len(f.written) != 0 {
		t.Fatalf("whitespace was treated as a region: %v", f.written)
	}
}

// The dedupe is not tidiness. scanNodes runs every 5 seconds and the error
// stream is capped at 500 entries and trims itself, so one misconfigured node
// reporting every tick would evict every other error in about forty minutes.
func TestAnUnknownRegionIsReportedOncePerDistinctValue(t *testing.T) {
	d, _ := newRegionDiscovery("eu")
	buf := captureLog(t)
	node := &models.Node{ID: 1, Name: "n1", Region: "default"}

	for i := 0; i < 5; i++ {
		d.applyHeartbeatRegion(node, "eu_cental")
	}
	if got := strings.Count(buf.String(), "eu_cental"); got != 1 {
		t.Fatalf("reported %d times, want 1:\n%s", got, buf.String())
	}

	// A DIFFERENT bad value is a different mistake and is reported again.
	d.applyHeartbeatRegion(node, "eu_centrl")
	if got := strings.Count(buf.String(), "eu_centrl"); got != 1 {
		t.Fatalf("a second distinct bad value was swallowed:\n%s", buf.String())
	}
}

// Once the operator fixes it, the node must be able to be reported again if it
// later regresses - otherwise the first typo silences the node forever.
func TestFixingTheRegionClearsTheReportedValue(t *testing.T) {
	d, _ := newRegionDiscovery("eu")
	captureLog(t)
	node := &models.Node{ID: 1, Name: "n1", Region: "default"}

	d.applyHeartbeatRegion(node, "eu_cental")
	d.applyHeartbeatRegion(node, "eu")
	if _, still := d.badRegionReported[node.ID]; still {
		t.Fatal("a node that now reports a valid region is still marked as bad")
	}
}

// A customer's BYON node may join an existing region but not create one: it
// sets NODE_REGION freely and would otherwise fill the platform's region list.
// An operator's External node is owned by the admin who enrolled it and may.
func TestOnlyTheOperatorsNodesCreateRegions(t *testing.T) {
	customer, admin := "cust", "op"
	cases := []struct {
		name    string
		owner   *string
		region  string
		created bool
		written bool
	}{
		{"platform node creates", nil, "us-east", true, true},
		{"admin-owned external node creates", &admin, "eu-central", true, true},
		{"customer node does not create", &customer, "tenant-made", false, false},
		{"customer node joins an existing region", &customer, "eu", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, f := newRegionDiscovery("eu")
			f.admins = map[string]bool{admin: true}
			captureLog(t)
			d.applyHeartbeatRegion(&models.Node{ID: 1, Name: "n1", Region: "default", OwnerID: tc.owner}, tc.region)
			if (len(f.ensured) > 0) != tc.created || (len(f.written) > 0) != tc.written {
				t.Errorf("ensured=%v written=%v, want created=%v written=%v", f.ensured, f.written, tc.created, tc.written)
			}
		})
	}
}

// Found live: node 325 held region "eu-central" in its column from before
// regions were created on report, and a matching column returned before the
// row was checked, so the row never appeared.
func TestARegionTheColumnAlreadyHoldsStillGetsItsRow(t *testing.T) {
	d, f := newRegionDiscovery()
	captureLog(t)
	d.applyHeartbeatRegion(&models.Node{ID: 1, Name: "n1", Region: "eu-central"}, "eu-central")
	if len(f.ensured) != 1 || len(f.written) != 0 {
		t.Fatalf("ensured=%v written=%v, want the row created and the column left alone", f.ensured, f.written)
	}
}

// Every 5s tick: a settled region costs no query, and a customer node with an
// unknown region is reported once, not on every tick.
func TestRepeatedHeartbeatsAreQuiet(t *testing.T) {
	d, f := newRegionDiscovery("eu")
	buf := captureLog(t)
	n := &models.Node{ID: 1, Name: "n1", Region: "eu"}
	for i := 0; i < 3; i++ {
		d.applyHeartbeatRegion(n, "eu")
	}
	if f.lookups != 1 {
		t.Errorf("region looked up %d times over three ticks, want 1", f.lookups)
	}

	cust := "cust"
	c := &models.Node{ID: 2, Name: "c1", Region: "default", OwnerID: &cust}
	f.admins = map[string]bool{}
	for i := 0; i < 3; i++ {
		d.applyHeartbeatRegion(c, "tenant-made")
	}
	if got := strings.Count(buf.String(), "customer node c1"); got != 1 {
		t.Errorf("customer report logged %d times over three ticks, want 1", got)
	}
}
