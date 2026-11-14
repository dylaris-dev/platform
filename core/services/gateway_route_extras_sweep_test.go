package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
	"dylaris-core/store"
)

// extrasStore is remoteFakeStore plus what the voice and Bedrock sweep reads
// and writes: the two column setters (stamping updated_at like the database),
// the stored Floodgate keys and the voice permission rows.
type extrasStore struct {
	*remoteFakeStore
	keys    map[int][]byte
	keyErr  error
	perms   map[string]bool
	permErr error
	// Failure knobs for the service paths that leave "provider on, local off".
	voiceSetErr, keySetErr, enqueueErr error
}

func (s *extrasStore) TouchRemoteRoute(domain string) error {
	s.setRow(domain, func(*store.RemoteRoute) {})
	return nil
}

func (s *extrasStore) SetBedrockKey(serverID int, key []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keySetErr != nil {
		return false, s.keySetErr
	}
	changed := !bytes.Equal(s.keys[serverID], key)
	s.keys[serverID] = append([]byte(nil), key...)
	return changed, nil
}

func (s *extrasStore) EnqueueOutbox(op, domain string, payload any) error {
	if s.enqueueErr != nil {
		return s.enqueueErr
	}
	return s.remoteFakeStore.EnqueueOutbox(op, domain, payload)
}

func (s *extrasStore) setRow(domain string, change func(*store.RemoteRoute)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.routes[domain]
	if !ok {
		return false
	}
	change(&r)
	r.UpdatedAt = time.Now()
	s.routes[domain] = r
	return true
}

func (s *extrasStore) SetRemoteRouteVoice(domain string, port *int, mod string) (bool, error) {
	if port != nil && s.voiceSetErr != nil {
		return false, s.voiceSetErr
	}
	return s.setRow(domain, func(r *store.RemoteRoute) { r.VoicePort, r.VoiceMod = port, mod }), nil
}

func (s *extrasStore) SetRemoteRouteEdition(domain, edition string) (bool, error) {
	return s.setRow(domain, func(r *store.RemoteRoute) { r.Edition = edition }), nil
}

func (s *extrasStore) GetBedrockKey(serverID int) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keyErr != nil {
		return nil, false, s.keyErr
	}
	k, ok := s.keys[serverID]
	return k, ok, nil
}

func (s *extrasStore) GetVoicePermission(scope string) (*bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.permErr != nil {
		return nil, s.permErr
	}
	if v, ok := s.perms[scope]; ok {
		return &v, nil
	}
	return nil, nil
}

func (s *extrasStore) GetSetting(key string) (string, error) {
	if key == VoiceChatSettingKey {
		return "true", nil
	}
	return "", nil
}

type extrasEnv struct {
	st   *extrasStore
	fake *remotegwtest.Fake
	c    *remotegw.Client
	kit  store.RemoteKit
	// published: the keys handed to the node's Redis, by server uuid.
	pubMu     sync.Mutex
	published map[string][]byte
	logs      *bytes.Buffer
	// later moves the sweep's clock on, as the next sweep's wait would.
	later func(time.Duration)
}

func newExtrasEnv(t *testing.T) *extrasEnv {
	t.Helper()
	st, fake, c, kit := newOutboxTest(t)
	e := &extrasEnv{st: &extrasStore{remoteFakeStore: st, keys: map[int][]byte{}, perms: map[string]bool{}},
		fake: fake, c: c, kit: kit, published: map[string][]byte{}, logs: &bytes.Buffer{}}
	resetExtrasSweep()
	clock := time.Duration(0)
	extrasNow = func() time.Time { return time.Now().Add(clock) }
	e.later = func(d time.Duration) { clock += d }
	t.Cleanup(func() { extrasNow = time.Now })
	prev := log.Writer()
	log.SetOutput(e.logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	return e
}

func (e *extrasEnv) publish(_ context.Context, uuid string, key []byte) (bool, error) {
	e.pubMu.Lock()
	defer e.pubMu.Unlock()
	changed := !bytes.Equal(e.published[uuid], key)
	e.published[uuid] = append([]byte(nil), key...)
	return changed, nil
}

// addServer puts server 100+i on node 9 (the kit's) with one address, on the
// provider and in the rows; the row's updated_at is long past.
func (e *extrasEnv) addServer(i int) (models.Server, string) {
	srv := models.Server{ID: 100 + i, UUID: fmt.Sprintf("0b6f1c2e-6a3d-4c1b-9e7f-%012d", i), NodeID: 9}
	domain := fmt.Sprintf("s%02d.example.com", i)
	e.st.mu.Lock()
	e.st.servers[srv.ID] = &srv
	e.st.routes[domain] = store.RemoteRoute{Domain: domain, ServerUUID: srv.UUID, NodeID: 9, Port: 25565}
	e.st.mu.Unlock()
	e.fake.AddRoute(e.kit.LinkID, domain, "mc-"+srv.UUID, 25565)
	return srv, domain
}

// voiceOn turns voice on at the provider and records its port in the row.
func (e *extrasEnv) voiceOn(t *testing.T, domain string) int {
	t.Helper()
	v, err := e.c.SetVoice(context.Background(), domain, true, VoiceInternalPort)
	if err != nil {
		t.Fatal(err)
	}
	e.setPort(domain, v.Port)
	return v.Port
}

func (e *extrasEnv) setPort(domain string, port int) {
	e.st.mu.Lock()
	defer e.st.mu.Unlock()
	r := e.st.routes[domain]
	r.VoicePort, r.VoiceMod = &port, "plasmo"
	e.st.routes[domain] = r
}

// bedrockOn gives the address Bedrock at the provider, stores its key for the
// server and records the edition, as AddBedrock does.
func (e *extrasEnv) bedrockOn(t *testing.T, srv models.Server, domain string, java bool) []byte {
	t.Helper()
	ctx := context.Background()
	if _, err := e.c.SetBedrock(ctx, domain, java, true); err != nil {
		t.Fatal(err)
	}
	key, err := e.c.FloodgateKey(ctx, domain)
	if err != nil {
		t.Fatal(err)
	}
	e.st.mu.Lock()
	defer e.st.mu.Unlock()
	e.st.keys[srv.ID] = key
	r := e.st.routes[domain]
	r.Edition = editionOf(java, true)
	e.st.routes[domain] = r
	return key
}

func (e *extrasEnv) sweep(t *testing.T) {
	t.Helper()
	if _, _, _, err := reconcileRemoteRoutes(context.Background(), e.st, e.c, nil, e.publish); err != nil {
		t.Fatal(err)
	}
}

func (e *extrasEnv) row(domain string) store.RemoteRoute {
	e.st.mu.Lock()
	defer e.st.mu.Unlock()
	return e.st.routes[domain]
}

// extrasWrites counts the voice and Bedrock writes sent to the provider.
func extrasWrites(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "PUT /routes/") && (strings.HasSuffix(c, "/voice") || strings.HasSuffix(c, "/bedrock")) {
			n++
		}
	}
	return n
}

// extrasKeyID names a key in a failure message without its bytes.
func extrasKeyID(k []byte) string {
	sum := sha256.Sum256(k)
	return "len " + strconv.Itoa(len(k)) + " sha256 " + hex.EncodeToString(sum[:4])
}

// openOps lists the open items as "op domain payload".
func openExtrasOps(st *extrasStore) []string {
	var out []string
	for _, it := range openOutbox(st.remoteFakeStore) {
		out = append(out, fmt.Sprintf("%s %s %+v", it.op, it.domain, it.payload))
	}
	return out
}

func switchedOffLines(logs *bytes.Buffer, domain string) int {
	n := 0
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, " "+domain+":") && strings.Contains(l, "switch it on again") {
			n++
		}
	}
	return n
}

// Review Focus 1: a provider whose own table read failed lists every address
// with voice off and Java only. The sweep re-asserts (at most 10 per sweep),
// the provider confirms what it really holds, and nothing is switched off.
func TestReconcileRouteExtrasSurvivesAListThatSaysOff(t *testing.T) {
	e := newExtrasEnv(t)
	ports := map[string]int{}
	keys := map[string][]byte{}
	for i := 0; i < 30; i++ {
		_, d := e.addServer(i)
		ports[d] = e.voiceOn(t, d)
	}
	for i := 30; i < 60; i++ {
		srv, d := e.addServer(i)
		keys[d] = e.bedrockOn(t, srv, d, i%2 == 0)
	}
	e.fake.SetListSaysOff(true)
	for sweep := 0; sweep < 7; sweep++ {
		before := len(e.fake.Calls())
		e.sweep(t)
		if n := extrasWrites(e.fake.Calls()[before:]); n > routeExtrasPerSweep {
			t.Fatalf("sweep %d sent %d voice/Bedrock writes, the budget is %d", sweep, n, routeExtrasPerSweep)
		} else if sweep == 0 && n != routeExtrasPerSweep {
			t.Fatalf("first sweep sent %d writes, want the whole budget %d", n, routeExtrasPerSweep)
		}
		for d, p := range ports {
			if r := e.row(d); r.VoicePort == nil || *r.VoicePort != p {
				t.Fatalf("sweep %d: voice of %s = %v, want %d", sweep, d, r.VoicePort, p)
			}
		}
		for d := range keys {
			if r := e.row(d); r.Edition == "" {
				t.Fatalf("sweep %d: Bedrock of %s switched off", sweep, d)
			}
		}
		if ops := openExtrasOps(e.st); len(ops) != 0 {
			t.Fatalf("sweep %d queued %v", sweep, ops)
		}
	}
	// A healed Bedrock address had its stored key republished, never another.
	healed := 0
	for i := 30; i < 60; i++ {
		srv := e.st.servers[100+i]
		d := fmt.Sprintf("s%02d.example.com", i)
		if got, ok := e.published[srv.UUID]; ok {
			healed++
			if !bytes.Equal(got, keys[d]) {
				t.Fatalf("%s: published %s, want %s", d, extrasKeyID(got), extrasKeyID(keys[d]))
			}
		}
	}
	if healed == 0 {
		t.Fatal("no Bedrock address was healed in seven sweeps")
	}
	if strings.Contains(e.logs.String(), "switch it on again") {
		t.Fatalf("a switch-off was logged:\n%s", e.logs.String())
	}
}

// The provider lost the address and it was put back (the route repair): it
// comes back with no voice and no editions, and the next sweep decides.
func TestReconcileRouteExtrasHealsAfterARepair(t *testing.T) {
	t.Run("voice with the same port stays on", func(t *testing.T) {
		e := newExtrasEnv(t)
		_, d := e.addServer(1)
		e.voiceOn(t, d)
		e.fake.DropRouteExtras(d)
		// The provider's pool hands the re-created address the next port; the
		// record here holds that one.
		e.setPort(d, 30001)
		e.sweep(t)
		if r := e.row(d); r.VoicePort == nil || *r.VoicePort != 30001 || len(openExtrasOps(e.st)) != 0 {
			t.Fatalf("row %+v, ops %v", r, openExtrasOps(e.st))
		}
	})
	t.Run("voice with another port goes off", func(t *testing.T) {
		e := newExtrasEnv(t)
		_, d := e.addServer(1)
		e.voiceOn(t, d)
		e.fake.DropRouteExtras(d)
		e.sweep(t)
		if r := e.row(d); r.VoicePort == nil || len(openExtrasOps(e.st)) != 0 {
			t.Fatalf("one answer switched voice off: row %+v, ops %v", r, openExtrasOps(e.st))
		}
		e.later(outboxSweepEvery)
		e.sweep(t)
		r := e.row(d)
		ops := openExtrasOps(e.st)
		if r.VoicePort != nil || r.VoiceMod != "" || len(ops) != 1 || ops[0] != "voice "+d+" {Enabled:false TargetPort:0}" {
			t.Fatalf("row %+v, ops %v", r, ops)
		}
		if n := switchedOffLines(e.logs, d); n != 1 {
			t.Fatalf("%d switch-off lines, want 1:\n%s", n, e.logs.String())
		}
	})
	t.Run("Bedrock with a new key adopts it (review M-1 ruling)", func(t *testing.T) {
		e := newExtrasEnv(t)
		srv, d := e.addServer(1)
		old := e.bedrockOn(t, srv, d, true)
		e.fake.DropRouteExtras(d)
		e.sweep(t)
		key := providerKeyOf(t, e, d)
		if bytes.Equal(key, old) {
			t.Fatal("the fake kept the key of the dropped address")
		}
		assertKeyRepaired(t, e, srv, d, key)
		if strings.Contains(e.logs.String(), hex.EncodeToString(old)) {
			t.Fatal("the log carries the old key")
		}
	})
	t.Run("a row that says Bedrock with no stored key gets the provider's", func(t *testing.T) {
		e := newExtrasEnv(t)
		srv, d := e.addServer(1)
		key := e.bedrockOn(t, srv, d, true)
		delete(e.st.keys, srv.ID) // the reset that failed after AddBedrock
		e.fake.SetListSaysOff(true)
		e.sweep(t)
		assertKeyRepaired(t, e, srv, d, key)
	})
}

// A refusal that stays until somebody changes something switches the
// feature off here, once it was answered in two sweeps a full interval apart;
// the provider's message stays out of the log line.
func TestReconcileRouteExtrasTurnsOffWhatTheProviderRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		voice  bool
		refuse func(f *remotegwtest.Fake)
	}{
		{"voice: voice_not_allowed", true, func(f *remotegwtest.Fake) { f.SetVoiceRefusal("voice_not_allowed") }},
		{"Bedrock: bedrock_disabled", false, func(f *remotegwtest.Fake) { f.SetBedrockRefusal("bedrock_disabled") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newExtrasEnv(t)
			srv, d := e.addServer(1)
			if tc.voice {
				e.voiceOn(t, d)
			} else {
				e.bedrockOn(t, srv, d, true)
			}
			// The provider really lost the extras; the re-assert is refused.
			e.fake.DropRouteExtras(d)
			tc.refuse(e.fake)
			e.sweep(t)
			e.later(outboxSweepEvery)
			e.sweep(t)
			r := e.row(d)
			ops := openExtrasOps(e.st)
			if r.VoicePort != nil || r.Edition != "" || len(ops) != 1 {
				t.Fatalf("row %+v, ops %v", r, ops)
			}
			if n := switchedOffLines(e.logs, d); n != 1 || strings.Contains(e.logs.String(), "fake") {
				t.Fatalf("%d switch-off lines, want 1 without the provider's message:\n%s", n, e.logs.String())
			}
		})
	}
}

// No answer about the address is no confirmation: 5xx, 429, a rejected key,
// an account-level 403, and a voice refusal about the moment (a link or relay
// down, a link being replaced, a full pool, an edge list that could not be
// read) change nothing; the next sweep asks again. A rejected key or a rate
// limit also ends this sweep's re-asserts.
func TestReconcileRouteExtrasLeavesRetryableErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(f *remotegwtest.Fake)
		// writes: provider writes this sweep for three mismatched addresses.
		writes int
	}{
		{"500", func(f *remotegwtest.Fake) { f.FailAfter(1, 3, 500, "internal", 0) }, 3},
		{"502 without a code", func(f *remotegwtest.Fake) { f.FailAfter(1, 3, 502, "", 0) }, 3},
		{"429", func(f *remotegwtest.Fake) { f.FailAfter(1, 3, 429, "rate_limited", time.Second) }, 1},
		{"401", func(f *remotegwtest.Fake) { f.FailAfter(1, 3, 401, "", 0) }, 1},
		{"403", func(f *remotegwtest.Fake) { f.FailAfter(1, 3, 403, "forbidden", 0) }, 1},
		{"link_without_voice", func(f *remotegwtest.Fake) { f.SetVoiceRefusal("link_without_voice") }, 3},
		{"no_relay", func(f *remotegwtest.Fake) { f.SetVoiceRefusal("no_relay") }, 3},
		{"no_routes", func(f *remotegwtest.Fake) { f.SetVoiceRefusal("no_routes") }, 3},
		{"voice_pool_full", func(f *remotegwtest.Fake) { f.SetVoiceRefusal("voice_pool_full") }, 3},
		{"voice kit_retiring", func(f *remotegwtest.Fake) { f.SetVoiceRefusal("kit_retiring") }, 3},
		{"voice not_found (contradicts the list read moments ago)", func(f *remotegwtest.Fake) { f.FailAfter(1, 3, 404, "not_found", 0) }, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newExtrasEnv(t)
			ports := map[string]int{}
			for i := 1; i <= 3; i++ {
				_, d := e.addServer(i)
				ports[d] = e.voiceOn(t, d)
				e.fake.DropRouteExtras(d)
			}
			tc.fail(e.fake)
			before := len(e.fake.Calls())
			e.sweep(t)
			if n := extrasWrites(e.fake.Calls()[before:]); n != tc.writes {
				t.Fatalf("%d provider writes, want %d: %v", n, tc.writes, e.fake.Calls()[before:])
			}
			for d, p := range ports {
				if r := e.row(d); r.VoicePort == nil || *r.VoicePort != p {
					t.Fatalf("voice of %s = %v, want %d", d, r.VoicePort, p)
				}
			}
			if ops := openExtrasOps(e.st); len(ops) != 0 {
				t.Fatalf("queued %v", ops)
			}
		})
	}
	t.Run("Bedrock: 503 on the key download", func(t *testing.T) {
		e := newExtrasEnv(t)
		srv, d := e.addServer(1)
		e.bedrockOn(t, srv, d, true)
		e.fake.SetListSaysOff(true)
		e.fake.FailAfter(2, 1, 503, "internal", 0) // the list and the PUT pass
		e.sweep(t)
		if r := e.row(d); r.Edition != "both" || len(openExtrasOps(e.st)) != 0 {
			t.Fatalf("row %+v, ops %v", r, openExtrasOps(e.st))
		}
	})
	t.Run("a network error or a deadline is no answer", func(t *testing.T) {
		for _, err := range []error{errors.New("dial tcp: i/o timeout"), context.DeadlineExceeded, remotegw.ErrBadAnswer} {
			if voiceRefusalIsFinal(err) || bedrockRefusalIsFinal(err) {
				t.Fatalf("%v read as a final refusal", err)
			}
		}
	})
}

// Review Focus 2: a row written moments ago may be a switch being flipped
// right now (the handler touches the row before it asks the provider).
func TestReconcileRouteExtrasSkipsARowTouchedMomentsAgo(t *testing.T) {
	e := newExtrasEnv(t)
	_, on := e.addServer(1)
	e.voiceOn(t, on)
	e.fake.DropRouteExtras(on)
	_, off := e.addServer(2)
	if _, err := e.c.SetVoice(context.Background(), off, true, VoiceInternalPort); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{on, off} {
		e.st.setRow(d, func(*store.RemoteRoute) {}) // touched now
	}
	before := len(e.fake.Calls())
	e.sweep(t)
	if n := extrasWrites(e.fake.Calls()[before:]); n != 0 {
		t.Fatalf("%d provider writes for rows touched moments ago", n)
	}
	if r := e.row(on); r.VoicePort == nil || len(openExtrasOps(e.st)) != 0 {
		t.Fatalf("row %+v, ops %v", r, openExtrasOps(e.st))
	}
}

// Review Focus 2: the rows are read before the provider list; a row changed
// in between is decided on its fresh copy, never on the one listed. The
// change here carries an old stamp, so only the comparison catches it.
func TestReconcileRouteExtrasRereadsTheRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, e *extrasEnv, srv models.Server, d string)
		change func(r *store.RemoteRoute)
	}{
		{"voice turned off meanwhile", func(t *testing.T, e *extrasEnv, _ models.Server, d string) { e.voiceOn(t, d) },
			func(r *store.RemoteRoute) { r.VoicePort, r.VoiceMod = nil, "" }},
		{"Bedrock taken off meanwhile", func(t *testing.T, e *extrasEnv, srv models.Server, d string) { e.bedrockOn(t, srv, d, true) },
			func(r *store.RemoteRoute) { r.Edition = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newExtrasEnv(t)
			srv, d := e.addServer(1)
			tc.setup(t, e, srv, d)
			e.fake.DropRouteExtras(d)
			e.st.afterListRoutes = func() {
				e.st.mu.Lock()
				defer e.st.mu.Unlock()
				r := e.st.routes[d]
				tc.change(&r)
				r.UpdatedAt = time.Now().Add(-time.Hour)
				e.st.routes[d] = r
				e.st.afterListRoutes = nil
			}
			before := len(e.fake.Calls())
			e.sweep(t)
			if n := extrasWrites(e.fake.Calls()[before:]); n != 0 {
				t.Fatalf("%d provider writes decided on the listed row", n)
			}
			if ops := openExtrasOps(e.st); len(ops) != 0 {
				t.Fatalf("queued %v", ops)
			}
		})
	}
}

// What the provider holds and the record here does not (an enable whose local
// write failed, a lost answer, an off whose enqueue failed) is switched off
// there, once: the item it queues keeps the address out of later sweeps.
func TestReconcileRouteExtrasProviderOnLocalOff(t *testing.T) {
	e := newExtrasEnv(t)
	ctx := context.Background()
	_, dv := e.addServer(1)
	if _, err := e.c.SetVoice(ctx, dv, true, VoiceInternalPort); err != nil {
		t.Fatal(err)
	}
	_, db := e.addServer(2)
	if _, err := e.c.SetBedrock(ctx, db, true, true); err != nil {
		t.Fatal(err)
	}
	e.sweep(t)
	want := []string{"voice " + dv + " {Enabled:false TargetPort:0}", "bedrock " + db + " {Java:true Bedrock:false}"}
	got := openExtrasOps(e.st)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ops %v, want %v", got, want)
	}
	e.sweep(t)
	if got := openExtrasOps(e.st); len(got) != 2 {
		t.Fatalf("second sweep while they are open: ops %v", got)
	}
	for i := 0; i < 2; i++ {
		drainAt(ctx, e.st, e.c, time.Now())
	}
	before := len(e.fake.Calls())
	e.sweep(t)
	if n := extrasWrites(e.fake.Calls()[before:]); n != 0 || len(openExtrasOps(e.st)) != 0 {
		t.Fatalf("after the drain: %d writes, ops %v", n, openExtrasOps(e.st))
	}
	if r := e.row(dv); r.VoicePort != nil {
		t.Fatalf("the sweep turned voice on here: %+v", r)
	}
}

// A voice permission taken away here takes effect at the next sweep.
func TestReconcileRouteExtrasRevokesVoiceWhenNoLongerAllowed(t *testing.T) {
	e := newExtrasEnv(t)
	srv, d := e.addServer(1)
	e.voiceOn(t, d)
	e.st.perms["server:"+strconv.Itoa(srv.ID)] = false
	before := len(e.fake.Calls())
	e.sweep(t)
	r := e.row(d)
	ops := openExtrasOps(e.st)
	if r.VoicePort != nil || len(ops) != 1 || ops[0] != "voice "+d+" {Enabled:false TargetPort:0}" {
		t.Fatalf("row %+v, ops %v", r, ops)
	}
	if n := extrasWrites(e.fake.Calls()[before:]); n != 0 {
		t.Fatalf("%d provider writes; the off goes through the outbox", n)
	}
	if n := switchedOffLines(e.logs, d); n != 1 {
		t.Fatalf("%d switch-off lines:\n%s", n, e.logs.String())
	}
}

func TestReconcileRouteExtrasKeepsVoiceOnAPermissionReadError(t *testing.T) {
	e := newExtrasEnv(t)
	_, d := e.addServer(1)
	p := e.voiceOn(t, d)
	e.st.permErr = errors.New("connection reset")
	e.sweep(t)
	if r := e.row(d); r.VoicePort == nil || *r.VoicePort != p || len(openExtrasOps(e.st)) != 0 {
		t.Fatalf("row %+v, ops %v", r, openExtrasOps(e.st))
	}
}

// An older provider lists neither voice nor editions: nothing said, nothing
// done, whatever the record here holds.
func TestReconcileRouteExtrasIgnoresAnOlderProvider(t *testing.T) {
	e := newExtrasEnv(t)
	srv, d := e.addServer(1)
	e.voiceOn(t, d)
	e.bedrockOn(t, srv, d, true)
	e.fake.DropRouteExtras(d)
	e.st.perms["server:"+strconv.Itoa(srv.ID)] = false
	budget := routeExtrasPerSweep
	before := len(e.fake.Calls())
	if err := reconcileRouteExtras(context.Background(), e.st, e.c, e.publish, e.row(d), remotegw.Route{Domain: d, LinkID: e.kit.LinkID}, srv, &budget); err != nil {
		t.Fatal(err)
	}
	if r := e.row(d); r.VoicePort == nil || r.Edition != "both" || len(openExtrasOps(e.st)) != 0 || len(e.fake.Calls()) != before || budget != routeExtrasPerSweep {
		t.Fatalf("row %+v, ops %v, calls %v, budget %d", r, openExtrasOps(e.st), e.fake.Calls()[before:], budget)
	}
}

// The sweep is the outbox loop's: it runs on the leader in remote mode only.
// In local mode, with the mode unknown, or on a follower it sends nothing and
// changes nothing, however far the record and the provider are apart.
func TestRouteExtrasSweepIdleUnlessRemoteLeader(t *testing.T) {
	for _, tc := range []struct {
		name          string
		leader        bool
		remote, known bool
		active        bool
	}{
		{"local mode", true, false, true, false},
		{"mode unknown", true, true, false, false},
		{"follower", false, true, true, false},
		{"remote leader", true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newExtrasEnv(t)
			_, on := e.addServer(1)
			e.voiceOn(t, on)
			e.fake.DropRouteExtras(on)
			_, off := e.addServer(2)
			if _, err := e.c.SetVoice(context.Background(), off, true, VoiceInternalPort); err != nil {
				t.Fatal(err)
			}
			l := &outboxLoop{st: e.st, api: func() *remotegw.Client { return e.c }, el: outboxLeader(tc.leader),
				mode: func() (bool, bool) { return tc.remote, tc.known }, publishKey: e.publish}
			before := len(e.fake.Calls())
			tickAt(l, time.Now())
			calls := e.fake.Calls()[before:]
			changed := e.row(on).VoicePort == nil || len(openExtrasOps(e.st)) > 0
			if !tc.active && (len(calls) != 0 || changed) {
				t.Fatalf("calls %v, row %+v, ops %v", calls, e.row(on), openExtrasOps(e.st))
			}
			if tc.active && (extrasWrites(calls) != 1 || !changed) {
				t.Fatalf("remote leader did not sweep: calls %v, ops %v", calls, openExtrasOps(e.st))
			}
		})
	}
}
