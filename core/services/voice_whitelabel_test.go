package services

import (
	"bytes"
	"context"
	"dylaris-core/models"
	"dylaris-core/store"
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

const (
	wlName  = "voice.play.example.com"
	wlOurs  = "voice.eu.dylaris.com"
	wlRoute = "join.shop.example.com"
	wlRName = "chat.shop.example.com"
)

// whiteLabelStore holds voice rows of both tables and address_dns_checks,
// stamped with the test clock. FallBack* is conditional like the real one.
type whiteLabelStore struct {
	store.Store
	mu       sync.Mutex
	now      func() time.Time
	ports    map[int]*store.VoicePort
	links    map[string]*store.LinkRouteVoice
	dns      map[string]store.AddressDNSCheck
	recorded int
	reads    int // GetAddressDNSChecks calls
	// onRecord runs after a check is stored, before the service acts on it.
	onRecord func()
}

func newWhiteLabelStore(now func() time.Time) *whiteLabelStore {
	return &whiteLabelStore{now: now, ports: map[int]*store.VoicePort{}, links: map[string]*store.LinkRouteVoice{}, dns: map[string]store.AddressDNSCheck{}}
}

func (f *whiteLabelStore) ListVoicePorts() ([]store.VoicePort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.VoicePort
	for _, p := range f.ports {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })
	return out, nil
}

func (f *whiteLabelStore) GetVoicePort(id int) (*store.VoicePort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.ports[id]; ok {
		c := *p
		return &c, nil
	}
	return nil, nil
}

func (f *whiteLabelStore) ListLinkRouteVoice() ([]store.LinkRouteVoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.LinkRouteVoice
	for _, v := range f.links {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, nil
}

func (f *whiteLabelStore) GetLinkRouteVoice(d string) (*store.LinkRouteVoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.links[d]; ok {
		c := *v
		return &c, nil
	}
	return nil, nil
}

func (f *whiteLabelStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: fmt.Sprintf("srv-%d", id), NodeID: 7, ActiveSubServer: "survival"}, nil
}

func (f *whiteLabelStore) GetAddressDNSChecks(names []string) (map[string]store.AddressDNSCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	out := map[string]store.AddressDNSCheck{}
	for _, n := range names {
		if c, ok := f.dns[n]; ok {
			out[n] = c
		}
	}
	return out, nil
}

func (f *whiteLabelStore) RecordAddressDNSCheck(name, status, region string, ok bool) (store.AddressDNSCheck, error) {
	f.mu.Lock()
	f.recorded++
	c := f.dns[name]
	c.Name, c.Status, c.Region, c.CheckedAt = name, status, region, f.now()
	if ok {
		c.Misses, c.FailingSince = 0, nil
	} else {
		c.Misses++
		if c.FailingSince == nil {
			at := c.CheckedAt
			c.FailingSince = &at
		}
	}
	f.dns[name] = c
	hook := f.onRecord
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return c, nil
}

func (f *whiteLabelStore) FallBackVoicePublicHost(id int, host string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.ports[id]
	if !ok || p.PublicHost != host || p.PublicState != store.VoicePublicActive {
		return false, nil
	}
	p.PublicState = store.VoicePublicFallenBack
	return true, nil
}

func (f *whiteLabelStore) FallBackLinkRouteVoicePublicHost(d, host string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.links[d]
	if !ok || v.PublicHost != host || v.PublicState != store.VoicePublicActive {
		return false, nil
	}
	v.PublicState = store.VoicePublicFallenBack
	return true, nil
}

func (f *whiteLabelStore) state(id int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ports[id].PublicState
}

type wlWrite struct {
	serverID int
	host     string
	port     int
}

type wlEnv struct {
	t      *testing.T
	svc    *VoiceService
	fs     *whiteLabelStore
	res    *fakePointingResolver
	clk    *pointingClock
	mr     *miniredis.Miniredis
	writes []wlWrite
}

// newWLEnv is a leader in local mode with server 7's voice row carrying the
// active name wlName in eu, last checked two hours ago (due).
func newWLEnv(t *testing.T) *wlEnv {
	t.Helper()
	res := newFakePointingResolver()
	pc, clk := newTestPointing(res, ptEdges())
	mr, rdb := newVoiceRedis(t)
	fs := newWhiteLabelStore(clk.Now)
	e := &wlEnv{t: t, fs: fs, res: res, clk: clk, mr: mr}
	e.addServer(7, wlName)
	e.svc = NewVoiceService(fs, rdb)
	e.svc.now = clk.Now
	e.svc.SetPointing(pc)
	e.svc.SetHostWriteBack(func(_ context.Context, srv *models.Server, host string, port int) {
		e.writes = append(e.writes, wlWrite{srv.ID, host, port})
	})
	return e
}

func (e *wlEnv) addServer(id int, name string) {
	e.fs.ports[id] = &store.VoicePort{
		ServerID: id, ServerUUID: fmt.Sprintf("srv-%d", id), Region: "eu", Port: 26000 + id, Mod: "svc",
		InternalPort: VoiceInternalPort, UplinkKey: bytes.Repeat([]byte{byte(id)}, 32), Host: wlOurs,
		PublicHost: name, PublicState: store.VoicePublicActive,
	}
	e.fs.dns[name] = store.AddressDNSCheck{Name: name, Status: "ok", Region: "eu", CheckedAt: e.clk.Now().Add(-2 * time.Hour)}
}

// point sets what the resolver answers for name: "ok" (CNAME to our eu
// name), "us" (CNAME to our us name: ok, but another region), "miss" (A to a
// foreign address) or "fail" (SERVFAIL).
func (e *wlEnv) point(name, mode string) {
	delete(e.res.cname, name)
	delete(e.res.a, name)
	delete(e.res.fail, "CNAME "+name)
	switch mode {
	case "ok":
		e.res.cname[name] = wlOurs
	case "us":
		e.res.cname[name] = "voice.us.dylaris.com"
	case "miss":
		e.res.a[name] = []netip.Addr{ptForeign}
	case "fail":
		e.res.fail["CNAME "+name] = errServFail
	}
}

func (e *wlEnv) tick(d time.Duration) {
	e.t.Helper()
	e.clk.Add(d)
	if err := e.svc.Reconcile(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// Three misses fall a name back only once they span 15 minutes; it falls back
// exactly once, with one write-back of OUR name, and is not checked again.
// Another region is a miss for voice.
func TestWhiteLabelFallsBackOnceAfterThreeMissesOver15Minutes(t *testing.T) {
	cases := []struct {
		name  string
		mode  string
		steps []time.Duration
		want  int
	}{
		{"three misses in 10 minutes", "miss", []time.Duration{0, 5 * time.Minute, 5 * time.Minute}, 0},
		{"three misses over 15 minutes", "miss", []time.Duration{0, 8 * time.Minute, 7 * time.Minute}, 1},
		{"another region over 15 minutes", "us", []time.Duration{0, 8 * time.Minute, 7 * time.Minute}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newWLEnv(t)
			e.point(wlName, c.mode)
			for _, d := range c.steps {
				e.tick(d)
			}
			wantState := store.VoicePublicActive
			if c.want == 1 {
				wantState = store.VoicePublicFallenBack
			}
			if got := e.fs.state(7); got != wantState {
				t.Fatalf("state %q, want %q", got, wantState)
			}
			if len(e.writes) != c.want {
				t.Fatalf("write-backs %v, want %d", e.writes, c.want)
			}
			if c.want == 1 && e.writes[0] != (wlWrite{7, wlOurs, 26007}) {
				t.Fatalf("write-back %+v, want our name and the port", e.writes[0])
			}
			if c.mode == "us" {
				// The shared row keeps the checker's answer for Bedrock.
				if got := e.fs.dns[wlName]; got.Status != "ok" || got.Region != "us" {
					t.Fatalf("stored %+v, want the raw ok in us", got)
				}
			}
			if c.want == 0 {
				return
			}
			calls := e.res.total()
			e.tick(5 * time.Minute) // a 4th tick
			if len(e.writes) != 1 || e.res.total() != calls {
				t.Fatalf("4th tick: writes %v, calls %d -> %d", e.writes, calls, e.res.total())
			}
		})
	}
}

// "Could not check" neither counts as a miss nor resets the run; an ok does.
func TestWhiteLabelLookupFailureNeitherCountsNorResets(t *testing.T) {
	type step struct {
		at       time.Duration // since the previous step
		mode     string
		fellBack bool
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"failures in between", []step{
			{0, "miss", false}, {5 * time.Minute, "fail", false}, {5 * time.Minute, "miss", false},
			// Counted, this failure would be the 4th miss over 15 minutes.
			{5 * time.Minute, "fail", false},
			{5 * time.Minute, "miss", true},
		}},
		{"an ok in between resets", []step{
			{0, "miss", false}, {5 * time.Minute, "miss", false}, {5 * time.Minute, "ok", false},
			// Without the reset this miss would be the 3rd, 70 minutes in.
			{time.Hour, "miss", false}, {5 * time.Minute, "miss", false}, {5 * time.Minute, "miss", false},
			{5 * time.Minute, "miss", true},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newWLEnv(t)
			for i, s := range c.steps {
				e.point(wlName, s.mode)
				before := e.fs.recorded
				e.tick(s.at)
				if s.mode == "fail" && e.fs.recorded != before {
					t.Fatalf("step %d: a failed lookup was stored", i)
				}
				if got := e.fs.state(7) == store.VoicePublicFallenBack; got != s.fellBack {
					t.Fatalf("step %d (%s): fallen back %v, want %v", i, s.mode, got, s.fellBack)
				}
			}
			if len(e.writes) != 1 {
				t.Fatalf("write-backs %v, want 1", e.writes)
			}
		})
	}
}

// A fallen back name stays fallen back when its record returns: only the
// customer's Check again moves it back.
func TestWhiteLabelNoAutomaticReturn(t *testing.T) {
	e := newWLEnv(t)
	e.point(wlName, "miss")
	for _, d := range []time.Duration{0, 8 * time.Minute, 7 * time.Minute} {
		e.tick(d)
	}
	if e.fs.state(7) != store.VoicePublicFallenBack {
		t.Fatal("did not fall back")
	}
	e.point(wlName, "ok")
	calls, recorded := e.res.total(), e.fs.recorded
	e.tick(2 * time.Hour)
	e.tick(24 * time.Hour)
	if e.fs.state(7) != store.VoicePublicFallenBack || len(e.writes) != 1 {
		t.Fatalf("state %q, writes %v", e.fs.state(7), e.writes)
	}
	if e.res.total() != calls || e.fs.recorded != recorded {
		t.Fatal("a fallen back name was checked")
	}
}

func TestWhiteLabelFollowerIdle(t *testing.T) {
	e := newWLEnv(t)
	e.point(wlName, "miss")
	e.svc.SetLeader(fakeLeader{leader: false})
	for _, d := range []time.Duration{0, 8 * time.Minute, 7 * time.Minute} {
		e.tick(d)
	}
	if e.res.total() != 0 || e.fs.recorded != 0 || e.fs.state(7) != store.VoicePublicActive {
		t.Fatalf("a follower checked: calls %d, recorded %d", e.res.total(), e.fs.recorded)
	}
}

func TestWhiteLabelIdleInRemoteMode(t *testing.T) {
	e := newWLEnv(t)
	e.point(wlName, "miss")
	e.svc.SetGatewayRemote(func() (bool, bool) { return true, true })
	for _, d := range []time.Duration{0, 8 * time.Minute, 7 * time.Minute} {
		e.tick(d)
	}
	if e.res.total() != 0 || e.fs.recorded != 0 || e.fs.state(7) != store.VoicePublicActive {
		t.Fatalf("remote mode checked: calls %d, recorded %d", e.res.total(), e.fs.recorded)
	}
}

// The fall back changes the name's state and the mod config, nothing else:
// the rows keep their port and key, the relay keys stay byte-identical. A
// route-only row changes its state only (Core cannot write that config).
func TestWhiteLabelFallBackKeepsPortAndKeys(t *testing.T) {
	e := newWLEnv(t)
	e.fs.links[wlRoute] = &store.LinkRouteVoice{
		Domain: wlRoute, OwnerID: "u1", Region: "eu", Port: 26100, TargetPort: 24454,
		UplinkKey: bytes.Repeat([]byte{0xcd}, 32), Host: wlOurs, PublicHost: wlRName, PublicState: store.VoicePublicActive,
	}
	e.fs.dns[wlRName] = store.AddressDNSCheck{Name: wlRName, Status: "ok", Region: "eu", CheckedAt: e.clk.Now().Add(-2 * time.Hour)}
	before := *e.fs.ports[7]
	routeBefore := *e.fs.links[wlRoute]

	e.point(wlName, "fail")
	e.point(wlRName, "fail")
	e.tick(0) // publishes, checks nothing
	keys := e.mr.Dump()

	e.point(wlName, "miss")
	e.point(wlRName, "miss")
	// The failed attempt above holds both names back for 5 minutes.
	for _, d := range []time.Duration{voiceNameRecheckFailing, 8 * time.Minute, 7 * time.Minute} {
		e.tick(d)
	}
	after, routeAfter := *e.fs.ports[7], *e.fs.links[wlRoute]
	if after.PublicState != store.VoicePublicFallenBack || routeAfter.PublicState != store.VoicePublicFallenBack {
		t.Fatalf("states %q / %q", after.PublicState, routeAfter.PublicState)
	}
	after.PublicState, routeAfter.PublicState = before.PublicState, routeBefore.PublicState
	if !bytes.Equal(after.UplinkKey, before.UplinkKey) || after.Port != before.Port || after.Host != before.Host || after.PublicHost != before.PublicHost {
		t.Fatalf("server row changed: %+v", after)
	}
	if !bytes.Equal(routeAfter.UplinkKey, routeBefore.UplinkKey) || routeAfter.Port != routeBefore.Port || routeAfter.Host != routeBefore.Host {
		t.Fatalf("route row changed: %+v", routeAfter)
	}
	if got := e.mr.Dump(); got != keys {
		t.Fatalf("relay keys changed:\n%s\nwant\n%s", got, keys)
	}
	if len(e.writes) != 1 || e.writes[0].serverID != 7 {
		t.Fatalf("write-backs %v, want one for the server only", e.writes)
	}
}

// The customer set another name between the check and the fall back: the
// conditional fall back matches nothing, so their name and config stay.
func TestWhiteLabelChangedNameIsNotOverwritten(t *testing.T) {
	e := newWLEnv(t)
	e.point(wlName, "miss")
	e.tick(0)
	e.tick(8 * time.Minute)
	e.fs.onRecord = func() {
		e.fs.mu.Lock()
		e.fs.ports[7].PublicHost = "talk.play.example.com"
		e.fs.mu.Unlock()
	}
	e.tick(7 * time.Minute)
	p := e.fs.ports[7]
	if p.PublicHost != "talk.play.example.com" || p.PublicState != store.VoicePublicActive {
		t.Fatalf("row %q %q, want the new name still active", p.PublicHost, p.PublicState)
	}
	if len(e.writes) != 0 {
		t.Fatalf("write-backs %v, want none", e.writes)
	}
}

// A resolver that never answers and 50 due names cannot hold the reconcile
// past the step's budget, and the orphan sweep before it still runs.
func TestVoiceReconcileWhiteLabelStepIsBounded(t *testing.T) {
	e := newWLEnv(t)
	for i := 8; i < 57; i++ {
		e.addServer(i, fmt.Sprintf("v%d.play.example.com", i))
	}
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	e.res.block = block
	orphan := store.VoicePort{ServerUUID: "99999999-0000-0000-0000-000000000000", Region: "us", Port: 26999, UplinkKey: make([]byte, 32)}
	if err := PublishVoice(context.Background(), e.svc.redis, orphan); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	e.tick(0)
	if d := time.Since(start); d > voiceNameStepBudget+2*time.Second {
		t.Fatalf("Reconcile took %s", d)
	}
	if e.mr.Exists("voice:port:us:26999") {
		t.Fatal("the orphan was not swept")
	}
	if e.fs.recorded != 0 {
		t.Fatal("a timed out lookup was stored")
	}
}

// Without an active name the reconcile makes no lookup, reads no check and
// writes the same Redis keys as without the step.
func TestVoiceReconcileUnchangedWithoutNames(t *testing.T) {
	run := func(wire bool) (string, *wlEnv) {
		e := newWLEnv(t)
		e.fs.ports[7].PublicHost, e.fs.ports[7].PublicState = "", ""
		e.addServer(8, "pending.play.example.com")
		e.fs.ports[8].PublicState = store.VoicePublicPending
		e.addServer(9, "fell.play.example.com")
		e.fs.ports[9].PublicState = store.VoicePublicFallenBack
		e.fs.links[wlRoute] = &store.LinkRouteVoice{Domain: wlRoute, OwnerID: "u1", Region: "eu", Port: 26100, UplinkKey: make([]byte, 32), Host: wlOurs}
		e.point("pending.play.example.com", "miss")
		e.point("fell.play.example.com", "miss")
		if !wire {
			e.svc.SetPointing(nil)
		}
		e.tick(2 * time.Hour)
		return e.mr.Dump(), e
	}
	without, _ := run(false)
	with, e := run(true)
	if with != without {
		t.Fatalf("keys differ:\n%s\nwant\n%s", with, without)
	}
	if e.res.total() != 0 || e.fs.reads != 0 || e.fs.recorded != 0 || len(e.writes) != 0 {
		t.Fatalf("calls %d, reads %d, recorded %d, writes %v", e.res.total(), e.fs.reads, e.fs.recorded, e.writes)
	}
}

// Final review I1: a name no live route covers any more (the route was
// deleted, or the own domain lapsed) is a miss from our own data, with no
// lookup, so the normal rule falls it back once. An admission that cannot be
// read stays "could not check".
func TestWhiteLabelUncoveredNameFallsBack(t *testing.T) {
	cases := []struct {
		name      string
		admit     func(context.Context, string) (bool, error)
		wantState string
		wantWrite int
	}{
		{"route removed", func(context.Context, string) (bool, error) { return false, nil }, store.VoicePublicFallenBack, 1},
		{"admission unreadable", func(context.Context, string) (bool, error) { return false, fmt.Errorf("redis down") }, store.VoicePublicActive, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newWLEnv(t)
			e.point(wlName, "ok") // DNS still answers: it must not be asked
			e.svc.pointing.SetAdmitted(c.admit)
			for _, d := range []time.Duration{0, 8 * time.Minute, 7 * time.Minute, 5 * time.Minute} {
				e.tick(d)
			}
			if got := e.fs.state(7); got != c.wantState {
				t.Fatalf("state %q, want %q", got, c.wantState)
			}
			if len(e.writes) != c.wantWrite {
				t.Fatalf("write-backs %v, want %d", e.writes, c.wantWrite)
			}
			if c.wantWrite == 1 && e.writes[0] != (wlWrite{7, wlOurs, 26007}) {
				t.Fatalf("write-back %+v, want our name and the port", e.writes[0])
			}
			if e.res.total() != 0 {
				t.Fatalf("%d lookups for a name no route covers", e.res.total())
			}
			if c.wantWrite == 0 && e.fs.recorded != 0 {
				t.Fatal("an unreadable admission was stored")
			}
		})
	}
}

// Final review I2: more names that never get an answer than the batch holds
// cannot starve the others. A name tried within 5 minutes is not due, so the
// second tick reaches the rest; an unknown answer is still never stored.
func TestWhiteLabelUnknownNamesDoNotStarveTheBatch(t *testing.T) {
	e := newWLEnv(t)
	e.point(wlName, "ok")
	var unknown, others []string
	for i := 8; i < 8+voiceNameBatch+5; i++ {
		n := fmt.Sprintf("dead%d.play.example.com", i)
		e.addServer(i, n)
		// Older than every other name, so they head the batch by checked_at.
		c := e.fs.dns[n]
		c.CheckedAt = e.clk.Now().Add(-3 * time.Hour)
		e.fs.dns[n] = c
		e.point(n, "fail")
		unknown = append(unknown, n)
	}
	others = append(others, wlName)
	for i := 100; i < 104; i++ {
		n := fmt.Sprintf("live%d.play.example.com", i)
		e.addServer(i, n)
		e.point(n, "ok")
		others = append(others, n)
	}
	e.tick(0)
	e.tick(time.Minute)
	now := e.clk.Now()
	for _, n := range others {
		if got := e.fs.dns[n].CheckedAt; now.Sub(got) > time.Minute {
			t.Fatalf("%s not re-checked within two ticks (checked %s ago)", n, now.Sub(got))
		}
	}
	for _, n := range unknown {
		if got := e.fs.dns[n]; got.Status != "ok" || now.Sub(got.CheckedAt) < 3*time.Hour {
			t.Fatalf("%s: an unknown answer was stored: %+v", n, got)
		}
	}
	// Every name was tried: a third tick within 5 minutes looks nothing up.
	calls := e.res.total()
	e.tick(time.Minute)
	if e.res.total() != calls {
		t.Fatalf("third tick made %d lookups, want 0", e.res.total()-calls)
	}
	// After 5 minutes the unknown names are tried again.
	e.tick(voiceNameRecheckFailing)
	if e.res.total() == calls {
		t.Fatal("the unknown names were never tried again")
	}
}
