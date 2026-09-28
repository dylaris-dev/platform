package services

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/store"
)

// selfRegStore is the narrow registrar surface, backed by maps.
type selfRegStore struct {
	regions map[string]store.WarpRegion
	leaders map[string]store.WarpLeader
	// upserts counts leader writes, so a test can prove a pass that agrees with
	// the registry writes NOTHING rather than rewriting the same row forever.
	upserts int
	// regionUpserts counts region writes, for the same reason and for proving a
	// failed read writes none.
	regionUpserts int
	// regionsErr makes the region read fail, which is the case a per-region Get
	// could not tell apart from "no such region".
	regionsErr error
}

func newSelfRegStore() *selfRegStore {
	return &selfRegStore{
		regions: map[string]store.WarpRegion{},
		leaders: map[string]store.WarpLeader{},
	}
}

func (s *selfRegStore) ListWarpRegions() ([]store.WarpRegion, error) {
	if s.regionsErr != nil {
		return nil, s.regionsErr
	}
	out := make([]store.WarpRegion, 0, len(s.regions))
	for _, r := range s.regions {
		out = append(out, r)
	}
	return out, nil
}

func (s *selfRegStore) UpsertWarpRegion(region, subnet string, enabled bool) error {
	s.regionUpserts++
	s.regions[region] = store.WarpRegion{Region: region, Subnet: subnet, Enabled: enabled}
	return nil
}

func (s *selfRegStore) ListWarpLeaders() ([]store.WarpLeader, error) {
	out := make([]store.WarpLeader, 0, len(s.leaders))
	for _, l := range s.leaders {
		out = append(out, l)
	}
	return out, nil
}

func (s *selfRegStore) UpsertWarpLeader(leaderID, region, endpoint string, enabled bool) error {
	s.upserts++
	s.leaders[leaderID] = store.WarpLeader{LeaderID: leaderID, Region: region, Endpoint: endpoint, Enabled: enabled}
	return nil
}

func selfRegFixture(t *testing.T) (*WarpSelfRegistrar, *selfRegStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	st := newSelfRegStore()
	return NewWarpSelfRegistrar(st, redis.NewClient(&redis.Options{Addr: mr.Addr()})), st, mr
}

func announce(t *testing.T, mr *miniredis.Miniredis, id string, a LeaderAnnouncement) {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mr.Set("dylaris:warp:"+id+":alive", string(b))
}

func liveAnnouncement() LeaderAnnouncement {
	return LeaderAnnouncement{V: 1, Region: "eu-central", Endpoint: "94.130.98.3:25599", Subnet: "10.77.0.0/16"}
}

// The whole point: a host added to the edge tier starts a leader with an id
// nobody has typed anywhere, and it becomes usable without an operator.
func TestSelfReg_RegistersAnUnknownLeader(t *testing.T) {
	r, st, mr := selfRegFixture(t)
	announce(t, mr, "eu-edge-02", liveAnnouncement())

	r.RunOnce(context.Background())

	l, ok := st.leaders["eu-edge-02"]
	if !ok {
		t.Fatal("leader was not registered")
	}
	if l.Region != "eu-central" || l.Endpoint != "94.130.98.3:25599" {
		t.Errorf("row = %+v", l)
	}
	if !l.Enabled {
		t.Error("a self-registered leader must be usable, not pending")
	}
	if reg, ok := st.regions["eu-central"]; !ok || reg.Subnet != "10.77.0.0/16" {
		t.Errorf("region = %+v, want it created from the leader's own subnet", reg)
	}
}

// An operator who disables a leader means it. Re-asserting Enabled on every
// pass would flip the switch back on within a minute, while the machine it
// belongs to is still running - which is exactly when someone reaches for it.
func TestSelfReg_KeepsAnOperatorsDisable(t *testing.T) {
	r, st, mr := selfRegFixture(t)
	st.regions["eu-central"] = store.WarpRegion{Region: "eu-central", Subnet: "10.77.0.0/16", Enabled: true}
	st.leaders["eu-edge-01"] = store.WarpLeader{LeaderID: "eu-edge-01", Region: "eu-central", Endpoint: "1.1.1.1:25599", Enabled: false}
	announce(t, mr, "eu-edge-01", liveAnnouncement())

	r.RunOnce(context.Background())

	l := st.leaders["eu-edge-01"]
	if l.Enabled {
		t.Error("a disabled leader was re-enabled by its own heartbeat")
	}
	if l.Endpoint != "94.130.98.3:25599" {
		t.Errorf("endpoint = %q, want the announced one (the address is still the leader's to report)", l.Endpoint)
	}
}

// A host that changes address must not need a human. The row follows.
func TestSelfReg_FollowsAMovedEndpoint(t *testing.T) {
	r, st, mr := selfRegFixture(t)
	st.regions["eu-central"] = store.WarpRegion{Region: "eu-central", Subnet: "10.77.0.0/16", Enabled: true}
	st.leaders["eu-edge-01"] = store.WarpLeader{LeaderID: "eu-edge-01", Region: "eu-central", Endpoint: "1.1.1.1:25599", Enabled: true}
	announce(t, mr, "eu-edge-01", liveAnnouncement())

	r.RunOnce(context.Background())

	if got := st.leaders["eu-edge-01"].Endpoint; got != "94.130.98.3:25599" {
		t.Errorf("endpoint = %q", got)
	}
}

// A pass that agrees with the registry must write nothing, or every leader row
// is rewritten once a minute forever.
func TestSelfReg_NoWriteWhenAlreadyCurrent(t *testing.T) {
	r, st, mr := selfRegFixture(t)
	st.regions["eu-central"] = store.WarpRegion{Region: "eu-central", Subnet: "10.77.0.0/16", Enabled: true}
	st.leaders["eu-edge-01"] = store.WarpLeader{LeaderID: "eu-edge-01", Region: "eu-central", Endpoint: "94.130.98.3:25599", Enabled: true}
	announce(t, mr, "eu-edge-01", liveAnnouncement())

	r.RunOnce(context.Background())

	if st.upserts != 0 {
		t.Errorf("upserts = %d, want 0", st.upserts)
	}
}

// Enrolled machines hold addresses out of the STORED subnet, and the leader
// bounds its peers by its own. Overwriting either strands one side, so a
// disagreement changes nothing at all.
func TestSelfReg_SubnetMismatch_ChangesNothing(t *testing.T) {
	r, st, mr := selfRegFixture(t)
	st.regions["eu-central"] = store.WarpRegion{Region: "eu-central", Subnet: "10.20.0.0/16", Enabled: true}
	announce(t, mr, "eu-edge-02", liveAnnouncement())

	r.RunOnce(context.Background())

	if _, ok := st.leaders["eu-edge-02"]; ok {
		t.Error("leader registered into a region whose subnet contradicts it")
	}
	if got := st.regions["eu-central"].Subnet; got != "10.20.0.0/16" {
		t.Errorf("stored subnet = %q, want it untouched", got)
	}
}

// Without a subnet there is nothing to create a region FROM, but the leader is
// still legitimate - it just cannot be the reason a region exists.
func TestSelfReg_NoSubnet_JoinsExistingRegionOnly(t *testing.T) {
	r, st, mr := selfRegFixture(t)
	a := liveAnnouncement()
	a.Subnet = ""
	announce(t, mr, "eu-edge-02", a)

	r.RunOnce(context.Background())
	if _, ok := st.leaders["eu-edge-02"]; ok {
		t.Fatal("registered into a region that does not exist")
	}

	st.regions["eu-central"] = store.WarpRegion{Region: "eu-central", Subnet: "10.77.0.0/16", Enabled: true}
	r.RunOnce(context.Background())
	if _, ok := st.leaders["eu-edge-02"]; !ok {
		t.Error("did not join the region once it existed")
	}
}

// A leader older than self-registration writes the literal "1", and so does a
// current one that cannot name its own address. Neither is an announcement, and
// the operator-entered row has to stand.
func TestSelfReg_IgnoresNonAnnouncements(t *testing.T) {
	r, st, mr := selfRegFixture(t)
	mr.Set("dylaris:warp:eu-edge-01:alive", "1")
	announce(t, mr, "eu-edge-03", LeaderAnnouncement{V: 99, Region: "eu-central", Endpoint: "9.9.9.9:25599", Subnet: "10.77.0.0/16"})
	announce(t, mr, "eu-edge-04", LeaderAnnouncement{V: 1, Region: "eu-central", Endpoint: "", Subnet: "10.77.0.0/16"})

	r.RunOnce(context.Background())

	if len(st.leaders) != 0 {
		t.Errorf("leaders = %+v, want none registered", st.leaders)
	}
}

// The namespace also holds the firewall allowlist and Core's region mirror.
// Neither is a leader, and reading one as an id would address a queue nothing
// listens on.
func TestSelfReg_IgnoresOtherKeysInTheNamespace(t *testing.T) {
	r, st, mr := selfRegFixture(t)
	mr.Set("dylaris:warp:firewall:allowed_ports", "25565")
	mr.Set("dylaris:warp:region:eu-central:subnet", "10.77.0.0/16")
	mr.Set("dylaris:warp:eu-edge-01:queue", "{}")

	r.RunOnce(context.Background())

	if len(st.leaders) != 0 || len(st.regions) != 0 {
		t.Errorf("leaders=%+v regions=%+v, want both empty", st.leaders, st.regions)
	}
}

// Every Core replica sees the same announcements. Without the gate they would
// all write the same rows on the same schedule.
func TestSelfReg_FollowerDoesNothing(t *testing.T) {
	r, st, mr := selfRegFixture(t)
	r.SetLeader(notTheLeader{})
	announce(t, mr, "eu-edge-02", liveAnnouncement())

	r.RunOnce(context.Background())

	if len(st.leaders) != 0 {
		t.Errorf("a follower wrote %+v", st.leaders)
	}
}

type notTheLeader struct{}

func (notTheLeader) IsLeader() bool { return false }

// The bug this shape exists to prevent. A per-region Get cannot tell "no such
// region" from "the read failed" - both arrive as an error - and the natural
// code falls through to the create branch, which is an UPSERT. One database
// blip would then rewrite a live region's subnet from whatever a leader
// happened to announce, stranding every machine holding an address out of the
// stored one, and re-enable a region an operator had switched off.
func TestSelfReg_RegionReadFails_WritesNothing(t *testing.T) {
	r, st, mr := selfRegFixture(t)
	st.regions["eu-central"] = store.WarpRegion{Region: "eu-central", Subnet: "10.20.0.0/16", Enabled: false}
	st.regionsErr = errRegionRead
	announce(t, mr, "eu-edge-02", liveAnnouncement())

	r.RunOnce(context.Background())

	if st.regionUpserts != 0 {
		t.Errorf("region upserts = %d, want 0 - a failed read is not an absent region", st.regionUpserts)
	}
	if got := st.regions["eu-central"]; got.Subnet != "10.20.0.0/16" || got.Enabled {
		t.Errorf("region = %+v, want it untouched", got)
	}
	if len(st.leaders) != 0 {
		t.Errorf("leaders = %+v, want none written on a pass that could not read", st.leaders)
	}
}

// Two leaders of the SAME new region in one pass must create it once. Reading
// the registry per announcement made that a race with itself.
func TestSelfReg_TwoLeadersOneNewRegion_CreateItOnce(t *testing.T) {
	r, st, mr := selfRegFixture(t)
	a := liveAnnouncement()
	announce(t, mr, "eu-edge-01", a)
	b := liveAnnouncement()
	b.Endpoint = "178.104.241.73:25599"
	announce(t, mr, "eu-edge-02", b)

	r.RunOnce(context.Background())

	if st.regionUpserts != 1 {
		t.Errorf("region upserts = %d, want exactly 1", st.regionUpserts)
	}
	if len(st.leaders) != 2 {
		t.Errorf("leaders = %+v, want both registered", st.leaders)
	}
}

var errRegionRead = errRegionReadType{}

type errRegionReadType struct{}

func (errRegionReadType) Error() string { return "connection reset by peer" }

// A leader whose announcement is GONE keeps its row. This is the invariant that
// had no test at all: the only thing preventing a "tidy up the orphans" pass was
// that warpRegistrarStore has no delete method, and anyone adding one would have
// added it to this fake in the same breath and seen nothing fail.
//
// It has to stay because Core removes a WireGuard peer by enumerating the leader
// rows of its region. Delete the row of a machine that is merely unreachable and
// its live peers are stranded on a host nothing can address any more. Liveness
// already sorts a dead leader last, which is the softer answer the design chose.
//
// A second leader IS announcing, because RunOnce returns early on a pass that
// read nothing - without it this would pass for the wrong reason.
func TestSelfReg_KeepsTheRowOfALeaderThatStoppedAnnouncing(t *testing.T) {
	r, st, mr := selfRegFixture(t)
	st.regions["eu-central"] = store.WarpRegion{Region: "eu-central", Subnet: "10.77.0.0/16", Enabled: true}
	// Two rows nothing announces for: one plain, one an operator switched off.
	st.leaders["eu-edge-09"] = store.WarpLeader{LeaderID: "eu-edge-09", Region: "eu-central", Endpoint: "203.0.113.9:25599", Enabled: true}
	st.leaders["eu-edge-10"] = store.WarpLeader{LeaderID: "eu-edge-10", Region: "eu-central", Endpoint: "203.0.113.10:25599", Enabled: false}
	announce(t, mr, "eu-edge-02", liveAnnouncement())

	r.RunOnce(context.Background())

	gone, ok := st.leaders["eu-edge-09"]
	if !ok {
		t.Fatal("the row of a leader that stopped announcing was removed; its peers are now unreachable")
	}
	if gone.Endpoint != "203.0.113.9:25599" || !gone.Enabled {
		t.Errorf("row = %+v, want it untouched", gone)
	}
	off, ok := st.leaders["eu-edge-10"]
	if !ok {
		t.Fatal("a disabled row with no announcement was removed")
	}
	if off.Enabled {
		t.Errorf("a silent leader was re-enabled: %+v", off)
	}
	// And the pass did its actual job, or the assertions above prove nothing.
	if _, ok := st.leaders["eu-edge-02"]; !ok {
		t.Error("the announcing leader was not registered, so this pass did nothing")
	}
}

// The wire shape of the liveness announcement, as a literal.
//
// gateway/warp WRITES this and platform/core READS it, and each repository
// compiles its OWN copy of the struct - so both test suites round-trip
// themselves and would stay green through a rename on one side, while
// self-registration stopped working in production. A golden literal is the only
// thing the two can both be held against.
//
// gateway/warp/announce_test.go pins the SAME string. Change one and you must
// change the other, which is the point: there is no version negotiation here.
const goldenLeaderAnnouncement = `{"v":1,"region":"eu-central","endpoint":"94.130.98.3:25599","subnet":"10.77.0.0/16"}`

func TestLeaderAnnouncementWireShapeIsFrozen(t *testing.T) {
	t.Run("core writes the golden shape", func(t *testing.T) {
		b, err := json.Marshal(liveAnnouncement())
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(b) != goldenLeaderAnnouncement {
			t.Errorf("\n got %s\nwant %s\nA field was renamed, retagged or reordered. The gateway leader on the other side is unchanged.", b, goldenLeaderAnnouncement)
		}
	})

	t.Run("core reads the golden shape", func(t *testing.T) {
		var a LeaderAnnouncement
		if err := json.Unmarshal([]byte(goldenLeaderAnnouncement), &a); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if a != liveAnnouncement() {
			t.Errorf("parsed %+v, want %+v", a, liveAnnouncement())
		}
	})

	// subnet is the one optional field: a leader that has none omits it rather
	// than sending "", because an empty subnet means "join the existing region"
	// and a missing region cannot be created from it.
	t.Run("a leader with no subnet omits the field", func(t *testing.T) {
		b, err := json.Marshal(LeaderAnnouncement{V: 1, Region: "eu-central", Endpoint: "94.130.98.3:25599"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		const want = `{"v":1,"region":"eu-central","endpoint":"94.130.98.3:25599"}`
		if string(b) != want {
			t.Errorf("\n got %s\nwant %s", b, want)
		}
	})
}
