package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"dylaris-pkg/voicewire"
)

const (
	voiceTestUUID  = "00112233-4455-6677-8899-aabbccddeeff"
	voiceTestUUID2 = "11111111-2222-3333-4444-555555555555"
	voiceTestKey   = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
)

// voiceHarness runs a real forwarder against a test "relay" socket and a
// test "container" socket, both on 127.0.0.1.
type voiceHarness struct {
	f       *voiceForwarder
	edge    *net.UDPConn
	backend *net.UDPConn

	mu      sync.Mutex
	running []string
	cfgs    map[string]*voiceConfig
	clock   time.Time

	cfgErr, lookupErr, resolveErr error
	resolveCalls                  int
}

func listenLoopback(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func voiceCfg(v int) *voiceConfig {
	return &voiceConfig{V: v, Region: "eu", Port: 26000, InternalPort: 24454, Key: voiceTestKey, Host: "a.eu.dylaris.com"}
}

func newVoiceHarness(t *testing.T, running []string, cfgs map[string]*voiceConfig) *voiceHarness {
	h := &voiceHarness{
		edge:    listenLoopback(t),
		backend: listenLoopback(t),
		running: running,
		cfgs:    cfgs,
		clock:   time.UnixMilli(1700000000000),
	}
	h.f = newVoiceForwarder(
		func() ([]MCContainer, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			var out []MCContainer
			for _, u := range h.running {
				out = append(out, MCContainer{UUID: u, ContainerName: "mc_" + u})
			}
			return out, nil
		},
		func(_ context.Context, uuid string) (*voiceConfig, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.cfgs[uuid], h.cfgErr
		},
		func(context.Context, string) ([]netip.Addr, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.lookupErr != nil {
				return nil, h.lookupErr
			}
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		func(string, int) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.resolveCalls++
			return h.backend.LocalAddr().String(), h.resolveErr
		},
		func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.clock },
	)
	h.f.uplinkPort = h.edge.LocalAddr().(*net.UDPAddr).Port
	t.Cleanup(h.f.shutdown)
	return h
}

func (h *voiceHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.clock = h.clock.Add(d)
	h.mu.Unlock()
}

func (h *voiceHarness) set(fn func()) {
	h.mu.Lock()
	fn()
	h.mu.Unlock()
}

func (h *voiceHarness) uplink() *net.UDPConn {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	return h.f.conn
}

func (h *voiceHarness) setRunning(r ...string) {
	h.mu.Lock()
	h.running = r
	h.mu.Unlock()
}

// read returns the next packet on c, or ok=false after d.
func read(c *net.UDPConn, d time.Duration) (b []byte, from *net.UDPAddr, ok bool) {
	buf := make([]byte, 2048)
	c.SetReadDeadline(time.Now().Add(d))
	n, from, err := c.ReadFromUDP(buf)
	if err != nil {
		return nil, nil, false
	}
	return buf[:n], from, true
}

// start scans, registers, and returns the forwarder's uplink address as the
// relay learns it: from the source of the REG.
func (h *voiceHarness) start(t *testing.T) *net.UDPAddr {
	t.Helper()
	h.f.scan(context.Background())
	h.f.register(context.Background())
	b, from, ok := read(h.edge, 2*time.Second)
	if !ok {
		t.Fatal("no REG reached the relay")
	}
	if f, err := voicewire.Parse(b); err != nil || f.Type != voicewire.TypeReg {
		t.Fatalf("first frame is not a REG: %x", b)
	}
	return from
}

func dataUp(t *testing.T, from *net.UDPConn, to *net.UDPAddr, uuid string, session uint32, payload string) {
	t.Helper()
	id, _ := voicewire.ParseServerUUID(uuid)
	if _, err := from.WriteToUDP(voicewire.AppendData(nil, voicewire.TypeDataUp, id, session, []byte(payload)), to); err != nil {
		t.Fatal(err)
	}
}

func (h *voiceHarness) sessionCount() int {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	return len(h.f.sessions)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestForwarderIsIdleWithoutKeys(t *testing.T) {
	h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{})
	h.f.scan(context.Background())
	h.f.register(context.Background())
	if h.f.conn != nil {
		t.Fatal("an uplink socket was opened without any voice key")
	}
	if b, _, ok := read(h.edge, 200*time.Millisecond); ok {
		t.Fatalf("a packet was sent without any voice key: %x", b)
	}
}

func TestForwarderSignsRegsTheRelayAccepts(t *testing.T) {
	h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(1)})
	h.f.scan(context.Background())
	h.f.register(context.Background())
	b, _, ok := read(h.edge, 2*time.Second)
	if !ok {
		t.Fatal("no REG")
	}
	f, err := voicewire.Parse(b)
	if err != nil || f.Type != voicewire.TypeReg {
		t.Fatalf("not a REG: %x (%v)", b, err)
	}
	if voicewire.FormatServerUUID(f.Server) != voiceTestUUID {
		t.Errorf("REG names %s", voicewire.FormatServerUUID(f.Server))
	}
	if f.UnixMs != 1700000000000 {
		t.Errorf("unix_ms %d is not the injected clock", f.UnixMs)
	}
	key, _ := hex.DecodeString(voiceTestKey)
	if !voicewire.VerifyReg(b, key) {
		t.Error("the relay would refuse this REG: MAC does not verify with the config key")
	}
}

func TestForwarderRegistersOnlyRunningServers(t *testing.T) {
	cfgs := map[string]*voiceConfig{voiceTestUUID: voiceCfg(1), voiceTestUUID2: voiceCfg(1)}
	h := newVoiceHarness(t, []string{voiceTestUUID, voiceTestUUID2}, cfgs)
	h.f.scan(context.Background())
	// The second server stops (a move stops the source first).
	h.setRunning(voiceTestUUID)
	h.f.scan(context.Background())
	h.f.register(context.Background())
	seen := map[string]int{}
	for {
		b, _, ok := read(h.edge, 300*time.Millisecond)
		if !ok {
			break
		}
		f, _ := voicewire.Parse(b)
		seen[voicewire.FormatServerUUID(f.Server)]++
	}
	if seen[voiceTestUUID] != 1 || seen[voiceTestUUID2] != 0 {
		t.Fatalf("REGs per server = %v, want only the running %s", seen, voiceTestUUID)
	}
}

func TestTwoPlayersGetTwoBackendSourcePorts(t *testing.T) {
	h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(1)})
	fwd := h.start(t)
	dataUp(t, h.edge, fwd, voiceTestUUID, 1, "a")
	dataUp(t, h.edge, fwd, voiceTestUUID, 2, "b")

	sources := map[int]string{}
	for range 2 {
		b, from, ok := read(h.backend, 2*time.Second)
		if !ok {
			t.Fatal("the backend did not get both packets")
		}
		sources[from.Port] = string(b)
		// Like SVC: reply to whoever sent it.
		h.backend.WriteToUDP(append([]byte("re:"), b...), from)
	}
	if len(sources) != 2 {
		t.Fatalf("two players arrived from %d source port(s): %v", len(sources), sources)
	}
	got := map[uint32]string{}
	for range 2 {
		b, _, ok := read(h.edge, 2*time.Second)
		if !ok {
			t.Fatal("a reply did not come back")
		}
		f, err := voicewire.Parse(b)
		if err != nil || f.Type != voicewire.TypeDataDown {
			t.Fatalf("reply is not DATA_DOWN: %x", b)
		}
		got[f.Session] = string(f.Payload)
	}
	if got[1] != "re:a" || got[2] != "re:b" {
		t.Fatalf("replies per session = %v", got)
	}
}

func TestSessionClosesAfterIdleAndOnClose(t *testing.T) {
	h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(1)})
	fwd := h.start(t)
	id, _ := voicewire.ParseServerUUID(voiceTestUUID)

	dataUp(t, h.edge, fwd, voiceTestUUID, 1, "x")
	waitFor(t, "session 1", func() bool { return h.sessionCount() == 1 })
	h.edge.WriteToUDP(voicewire.AppendClose(nil, id, 1), fwd)
	waitFor(t, "CLOSE to end session 1", func() bool { return h.sessionCount() == 0 })

	dataUp(t, h.edge, fwd, voiceTestUUID, 2, "y")
	waitFor(t, "session 2", func() bool { return h.sessionCount() == 1 })
	h.f.sweep() // stamps the last DATA_UP
	h.advance(voiceSessionIdle - time.Second)
	h.f.sweep()
	if h.sessionCount() != 1 {
		t.Fatal("a session closed before it was idle for voiceSessionIdle")
	}
	h.advance(time.Second)
	h.f.sweep()
	if h.sessionCount() != 0 {
		t.Fatal("a session idle for voiceSessionIdle is still open")
	}
}

func TestForwarderIgnoresFramesFromNonEdgeAddresses(t *testing.T) {
	h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(1)})
	fwd := h.start(t)
	stranger := listenLoopback(t)
	dataUp(t, stranger, fwd, voiceTestUUID, 7, "forged")
	dataUp(t, h.edge, fwd, voiceTestUUID, 1, "real")
	b, _, ok := read(h.backend, 2*time.Second)
	if !ok || string(b) != "real" {
		t.Fatalf("first packet at the backend = %q, want the edge's", b)
	}
	if b, _, ok := read(h.backend, 200*time.Millisecond); ok {
		t.Fatalf("a frame from a non-edge address reached the backend: %q", b)
	}
}

func TestForwarderIgnoresUnknownVersion(t *testing.T) {
	t.Run("a v2 config opens nothing", func(t *testing.T) {
		h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(2)})
		h.f.scan(context.Background())
		h.f.register(context.Background())
		if h.f.conn != nil {
			t.Fatal("a v2 config opened the uplink socket")
		}
		if _, _, ok := read(h.edge, 200*time.Millisecond); ok {
			t.Fatal("a v2 config sent a packet")
		}
	})
	t.Run("a ver=2 frame is dropped", func(t *testing.T) {
		h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(1)})
		fwd := h.start(t)
		id, _ := voicewire.ParseServerUUID(voiceTestUUID)
		v2 := voicewire.AppendData(nil, voicewire.TypeDataUp, id, 1, []byte("v2"))
		v2[2] = 2
		h.edge.WriteToUDP(v2, fwd)
		dataUp(t, h.edge, fwd, voiceTestUUID, 1, "v1")
		if b, _, ok := read(h.backend, 2*time.Second); !ok || string(b) != "v1" {
			t.Fatalf("first packet at the backend = %q, want v1", b)
		}
	})
}

func TestSessionCapPerServer(t *testing.T) {
	h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(1)})
	h.f.maxSessions = 2
	fwd := h.start(t)
	dataUp(t, h.edge, fwd, voiceTestUUID, 1, "1")
	dataUp(t, h.edge, fwd, voiceTestUUID, 2, "2")
	dataUp(t, h.edge, fwd, voiceTestUUID, 3, "3")
	dataUp(t, h.edge, fwd, voiceTestUUID, 1, "probe")
	var got []string
	for {
		b, _, ok := read(h.backend, 300*time.Millisecond)
		if !ok {
			break
		}
		got = append(got, string(b))
	}
	if strings.Contains(strings.Join(got, ","), "3") {
		t.Fatalf("a session over the cap reached the backend: %v", got)
	}
	if len(got) != 3 || h.sessionCount() != 2 {
		t.Fatalf("backend got %v with %d sessions, want 3 packets over 2 sessions", got, h.sessionCount())
	}
}

// The platform half of the voice seam (spec 3.13): a stub relay sends the
// Task 2 golden DATA_UP as literal bytes, and the reply must be the golden
// DATA_DOWN. The gateway half drives its real relay with the same goldens.
func TestForwarderAgainstAReplayedRelay(t *testing.T) {
	h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(1)})
	fwd := h.start(t)
	up, _ := hex.DecodeString("d756010300112233445566778899aabbccddeeff0a0b0c0d6869")
	h.edge.WriteToUDP(up, fwd)
	b, from, ok := read(h.backend, 2*time.Second)
	if !ok || string(b) != "hi" {
		t.Fatalf("backend got %q, want hi", b)
	}
	h.backend.WriteToUDP([]byte("hi"), from)
	down, _, ok := read(h.edge, 2*time.Second)
	want, _ := hex.DecodeString("d756010400112233445566778899aabbccddeeff0a0b0c0d6869")
	if !ok || !bytes.Equal(down, want) {
		t.Fatalf("reply = %x, want %x", down, want)
	}
}

func TestScanFailuresKeepWhatTheLastScanFound(t *testing.T) {
	for _, c := range []struct {
		name string
		fail func(h *voiceHarness)
	}{
		{"a DNS failure keeps the previous edges", func(h *voiceHarness) { h.lookupErr = errors.New("no such host") }},
		{"a Redis error keeps the previous server", func(h *voiceHarness) { h.cfgErr = errors.New("connection refused") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(1)})
			fwd := h.start(t)
			dataUp(t, h.edge, fwd, voiceTestUUID, 1, "a")
			waitFor(t, "session 1", func() bool { return h.sessionCount() == 1 })
			h.set(func() { c.fail(h) })
			h.f.scan(context.Background())
			if h.sessionCount() != 1 || h.uplink() == nil {
				t.Fatalf("the scan failure tore voice down: %d sessions, uplink %v", h.sessionCount(), h.uplink())
			}
			h.f.register(context.Background())
			if _, _, ok := read(h.edge, 2*time.Second); !ok {
				t.Fatal("no REG after the scan failure")
			}
		})
	}
}

func TestBackendResolveFailureCostsNoDockerCallPerPacket(t *testing.T) {
	h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(1)})
	h.resolveErr = errors.New("container not running")
	fwd := h.start(t)
	for i := range 20 {
		dataUp(t, h.edge, fwd, voiceTestUUID, uint32(i), "x")
	}
	if b, _, ok := read(h.backend, 300*time.Millisecond); ok {
		t.Fatalf("a packet reached an unresolved backend: %q", b)
	}
	h.mu.Lock()
	calls := h.resolveCalls
	h.mu.Unlock()
	if calls != 1 {
		t.Fatalf("resolve called %d times for one scan and 20 packets, want 1", calls)
	}
	if h.sessionCount() != 0 {
		t.Fatalf("%d sessions opened without a backend", h.sessionCount())
	}
}

func TestUplinkClosesAndReopensWithTheServerSet(t *testing.T) {
	h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(1)})
	h.start(t)
	first := h.uplink()
	h.setRunning()
	h.f.scan(context.Background())
	if h.uplink() != nil {
		t.Fatal("the uplink socket stayed open with no voice server left")
	}
	h.f.register(context.Background())
	if _, _, ok := read(h.edge, 200*time.Millisecond); ok {
		t.Fatal("a REG was sent with no voice server left")
	}
	h.setRunning(voiceTestUUID)
	fwd := h.start(t)
	if h.uplink() == nil || h.uplink() == first {
		t.Fatal("the uplink socket did not reopen")
	}
	dataUp(t, h.edge, fwd, voiceTestUUID, 1, "back")
	if b, _, ok := read(h.backend, 2*time.Second); !ok || string(b) != "back" {
		t.Fatalf("backend got %q after the reopen", b)
	}
}

// One failed Docker resolve must not cut voice for a scan period: the backend
// the last scan found stays.
func TestBackendResolveFailureKeepsThePreviousBackend(t *testing.T) {
	h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(1)})
	fwd := h.start(t)
	h.set(func() { h.resolveErr = errors.New("docker timed out") })
	h.f.scan(context.Background())
	dataUp(t, h.edge, fwd, voiceTestUUID, 1, "still")
	if b, _, ok := read(h.backend, 2*time.Second); !ok || string(b) != "still" {
		t.Fatalf("backend got %q after a failed resolve, want the packet", b)
	}
}

// A container that came back at another address: its sessions were dialed to
// the old one, so they close and the next packet opens one to the new one.
func TestBackendAddressChangeClosesTheSessions(t *testing.T) {
	h := newVoiceHarness(t, []string{voiceTestUUID}, map[string]*voiceConfig{voiceTestUUID: voiceCfg(1)})
	fwd := h.start(t)
	dataUp(t, h.edge, fwd, voiceTestUUID, 1, "a")
	if _, _, ok := read(h.backend, 2*time.Second); !ok {
		t.Fatal("first packet did not arrive")
	}
	moved := listenLoopback(t)
	h.set(func() { h.backend = moved })
	h.f.scan(context.Background())
	if n := h.sessionCount(); n != 0 {
		t.Fatalf("%d session(s) survived a backend address change", n)
	}
	dataUp(t, h.edge, fwd, voiceTestUUID, 1, "b")
	if b, _, ok := read(moved, 2*time.Second); !ok || string(b) != "b" {
		t.Fatalf("new backend got %q, want b", b)
	}
}
