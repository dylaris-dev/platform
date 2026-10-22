package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"dylaris-pkg/voicewire"

	"github.com/redis/go-redis/v9"
)

// The voice forwarder carries proximity voice chat between the gateway voice
// relays and the MC containers. It dials OUT: it REGs every 10 s to each edge
// of a server's route domain, which also keeps a NAT mapping open, so a BYON
// node behind NAT needs no inbound port. Without a dylaris:server:<uuid>:voice
// key it opens no socket and sends nothing, so an old Core changes nothing.
const (
	voiceUplinkPort           = 25562
	voiceSessionIdle          = 30 * time.Second
	voiceMaxSessionsPerServer = 512

	voiceScanEvery     = 30 * time.Second
	voiceRegisterEvery = 10 * time.Second
	voiceResolveWait   = 3 * time.Second
)

type voiceConfig struct {
	V            int    `json:"v"`
	Region       string `json:"region"`
	Port         int    `json:"port"`
	InternalPort int    `json:"internal_port"`
	Key          string `json:"key"`
	Host         string `json:"host"`
}

// key returns the uplink key, or false for a config this node must ignore: an
// unknown version is a shape it cannot read, not one to guess at.
func (c *voiceConfig) key() ([]byte, bool) {
	if c == nil || c.V != 1 || len(c.Key) != 64 || c.Host == "" || c.InternalPort < 1 || c.InternalPort > 65535 {
		return nil, false
	}
	k, err := hex.DecodeString(c.Key)
	return k, err == nil
}

type voiceServer struct {
	uuid string
	key  []byte
	// backend is the container's voice address, resolved by the scan so the
	// packet path never asks Docker. nil means it did not resolve, and that
	// server's packets are dropped until a later scan finds it.
	backend *net.UDPAddr
	// edges are the only addresses frames are accepted from, and the only
	// ones REGs go to. Anything else on the uplink socket is a stranger.
	edges []netip.AddrPort
}

func (s *voiceServer) backendOrNil() *net.UDPAddr {
	if s == nil {
		return nil
	}
	return s.backend
}

func sameUDPAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.IP.Equal(b.IP) && a.Port == b.Port
}

func (s *voiceServer) isEdge(a netip.AddrPort) bool {
	for _, e := range s.edges {
		if e == a {
			return true
		}
	}
	return false
}

type voiceSessionKey struct {
	edge    netip.AddrPort
	server  [16]byte
	session uint32
}

type voiceSession struct {
	backend *net.UDPConn
	// active is set by every DATA_UP and cleared by the sweep, so the hot path
	// never reads a clock. A new session is active, so the sweep stamps
	// lastSeen before it ever compares it.
	active   atomic.Bool
	lastSeen time.Time
}

type voiceForwarder struct {
	listRunning func() ([]MCContainer, error)
	getConfig   func(ctx context.Context, uuid string) (*voiceConfig, error)
	lookup      func(ctx context.Context, host string) ([]netip.Addr, error)
	resolve     func(uuid string, port int) (string, error)
	now         func() time.Time

	uplinkPort  int
	maxSessions int

	mu       sync.Mutex
	conn     *net.UDPConn
	servers  map[[16]byte]*voiceServer
	sessions map[voiceSessionKey]*voiceSession

	errMu   sync.Mutex
	lastErr string
}

func newVoiceForwarder(
	listRunning func() ([]MCContainer, error),
	getConfig func(ctx context.Context, uuid string) (*voiceConfig, error),
	lookup func(ctx context.Context, host string) ([]netip.Addr, error),
	resolve func(uuid string, port int) (string, error),
	now func() time.Time,
) *voiceForwarder {
	return &voiceForwarder{
		listRunning: listRunning,
		getConfig:   getConfig,
		lookup:      lookup,
		resolve:     resolve,
		now:         now,
		uplinkPort:  voiceUplinkPort,
		maxSessions: voiceMaxSessionsPerServer,
		servers:     map[[16]byte]*voiceServer{},
		sessions:    map[voiceSessionKey]*voiceSession{},
	}
}

func startVoiceForwarder(ctx context.Context, rdb *redis.Client, dm *DockerManager, nodeID string) {
	clock := heartbeatClock(rdb, nodeID)
	f := newVoiceForwarder(
		dm.ListRunningMCContainers,
		func(ctx context.Context, uuid string) (*voiceConfig, error) {
			raw, err := rdb.Get(ctx, "dylaris:server:"+uuid+":voice").Bytes()
			if errors.Is(err, redis.Nil) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			var c voiceConfig
			if err := json.Unmarshal(raw, &c); err != nil {
				return nil, fmt.Errorf("voice config of %s: %w", uuid, err)
			}
			return &c, nil
		},
		func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		},
		resolveMCAddr,
		// The relay refuses a REG more than 60 s off its clock, and a
		// customer's machine is the one that drifts.
		func() time.Time { return clock.now(ctx) },
	)
	f.run(ctx)
}

func (f *voiceForwarder) run(ctx context.Context) {
	defer f.shutdown()
	f.scan(ctx)
	f.register(ctx)
	scanT := time.NewTicker(voiceScanEvery)
	regT := time.NewTicker(voiceRegisterEvery)
	defer scanT.Stop()
	defer regT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-scanT.C:
			f.scan(ctx)
		case <-regT.C:
			f.register(ctx)
			f.sweep()
		}
	}
}

func (f *voiceForwarder) logErr(err error) {
	// One line per distinct failure, not one every 30 s for as long as it lasts.
	f.errMu.Lock()
	defer f.errMu.Unlock()
	if msg := err.Error(); msg != f.lastErr {
		f.lastErr = msg
		log.Printf("voice: %v", err)
	}
}

// scan rebuilds the server set from the RUNNING containers only. A move stops
// the source first, so the old node falls silent instead of alternating with
// the new one as the relay's latest REG.
func (f *voiceForwarder) scan(ctx context.Context) {
	running, err := f.listRunning()
	if err != nil {
		f.logErr(fmt.Errorf("list running servers: %w", err))
		return
	}
	f.mu.Lock()
	prev := f.servers
	f.mu.Unlock()

	next := map[[16]byte]*voiceServer{}
	hosts := map[string]voiceLookup{}
	for _, c := range running {
		id := voicewire.ServerID(c.UUID)
		cfg, err := f.getConfig(ctx, c.UUID)
		if err != nil {
			// A Redis blip is not a disable: keep what the last scan found.
			f.logErr(err)
			if prev[id] != nil {
				next[id] = prev[id]
			}
			continue
		}
		key, ok := cfg.key()
		if !ok {
			continue
		}
		res, done := hosts[cfg.Host]
		if !done {
			res = f.lookupEdges(ctx, cfg.Host)
			hosts[cfg.Host] = res
			if res.err != nil {
				f.logErr(fmt.Errorf("resolve %s: %w", cfg.Host, res.err))
			}
		}
		s := &voiceServer{uuid: c.UUID, key: key, edges: res.edges, backend: f.resolveBackend(c.UUID, cfg.InternalPort)}
		// A failed lookup keeps the edges it had rather than dropping voice
		// on one DNS hiccup; a failed Docker resolve keeps the backend the
		// same way.
		if len(s.edges) == 0 && prev[id] != nil {
			s.edges = prev[id].edges
		}
		if s.backend == nil && prev[id] != nil {
			s.backend = prev[id].backend
		}
		if len(s.edges) > 0 {
			next[id] = s
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers = next
	for k, sess := range f.sessions {
		// A session is dialed to the backend it opened with; one whose
		// container came back at another address would talk to nobody.
		if srv := next[k.server]; srv == nil || !srv.isEdge(k.edge) || !sameUDPAddr(srv.backend, prev[k.server].backendOrNil()) {
			f.closeSessionLocked(k, sess)
		}
	}
	switch {
	case len(next) == 0 && f.conn != nil:
		f.conn.Close()
		f.conn = nil
	case len(next) > 0 && f.conn == nil:
		conn, err := net.ListenUDP("udp4", nil)
		if err != nil {
			f.logErr(fmt.Errorf("open uplink socket: %w", err))
			return
		}
		f.conn = conn
		go f.serveUplink(conn)
	}
}

type voiceLookup struct {
	edges []netip.AddrPort
	err   error
}

func (f *voiceForwarder) lookupEdges(ctx context.Context, host string) (res voiceLookup) {
	lctx, cancel := context.WithTimeout(ctx, voiceResolveWait)
	defer cancel()
	addrs, err := f.lookup(lctx, host)
	res.err = err
	for _, a := range addrs {
		if a = a.Unmap(); a.Is4() {
			res.edges = append(res.edges, netip.AddrPortFrom(a, uint16(f.uplinkPort)))
		}
	}
	return res
}

// resolveBackend asks Docker for the container's address, bounded so a hung
// daemon delays one scan instead of wedging the forwarder.
func (f *voiceForwarder) resolveBackend(uuid string, port int) *net.UDPAddr {
	type result struct {
		addr string
		err  error
	}
	ch := make(chan result, 1)
	// ponytail: a hung Docker call leaks this goroutine until it returns; the
	// resolver takes no context.
	go func() {
		a, err := f.resolve(uuid, port)
		ch <- result{a, err}
	}()
	var r result
	select {
	case r = <-ch:
	case <-time.After(voiceResolveWait):
		r.err = errors.New("timed out")
	}
	if r.err != nil {
		f.logErr(fmt.Errorf("voice backend of %s: %w", uuid, r.err))
		return nil
	}
	ua, err := net.ResolveUDPAddr("udp4", r.addr)
	if err != nil {
		f.logErr(fmt.Errorf("voice backend of %s: %w", uuid, err))
		return nil
	}
	return ua
}

// register sends a signed REG for every server to every one of its edges.
func (f *voiceForwarder) register(ctx context.Context) {
	f.mu.Lock()
	conn := f.conn
	servers := make([]*voiceServer, 0, len(f.servers))
	for _, s := range f.servers {
		servers = append(servers, s)
	}
	f.mu.Unlock()
	if conn == nil {
		return
	}
	ms := uint64(f.now().UnixMilli())
	var buf []byte
	for _, s := range servers {
		id := voicewire.ServerID(s.uuid)
		for _, e := range s.edges {
			var nonce [8]byte
			_, _ = rand.Read(nonce[:])
			buf = voicewire.AppendReg(buf[:0], id, ms, nonce, s.key)
			_, _ = conn.WriteToUDPAddrPort(buf, e)
		}
	}
}

// serveUplink reads frames from the relays until conn is closed.
func (f *voiceForwarder) serveUplink(conn *net.UDPConn) {
	buf := make([]byte, 64*1024)
	for {
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		src = netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
		fr, err := voicewire.Parse(buf[:n])
		if err != nil {
			continue
		}
		switch fr.Type {
		case voicewire.TypeDataUp:
			f.dataUp(conn, src, fr)
		case voicewire.TypeClose:
			f.mu.Lock()
			k := voiceSessionKey{src, fr.Server, fr.Session}
			if sess := f.sessions[k]; sess != nil {
				f.closeSessionLocked(k, sess)
			}
			f.mu.Unlock()
		}
		// REG_ACK needs no action: the REG every 10 s is the retry.
	}
}

func (f *voiceForwarder) dataUp(conn *net.UDPConn, src netip.AddrPort, fr voicewire.Frame) {
	k := voiceSessionKey{src, fr.Server, fr.Session}
	f.mu.Lock()
	srv := f.servers[fr.Server]
	if srv == nil || !srv.isEdge(src) || srv.backend == nil {
		f.mu.Unlock()
		return
	}
	sess := f.sessions[k]
	if sess == nil {
		n := 0
		for o := range f.sessions {
			if o.server == fr.Server {
				n++
			}
		}
		if n >= f.maxSessions {
			f.mu.Unlock()
			return
		}
		var err error
		if sess, err = f.openSession(conn, k, srv); err != nil {
			f.logErr(err)
			f.mu.Unlock()
			return
		}
		f.sessions[k] = sess
	}
	sess.active.Store(true)
	f.mu.Unlock()
	_, _ = sess.backend.Write(fr.Payload)
}

// openSession dials the container from a socket of its own, so the voice mod,
// which tells players apart by source address, sees one sender per player.
func (f *voiceForwarder) openSession(conn *net.UDPConn, k voiceSessionKey, srv *voiceServer) (*voiceSession, error) {
	backend, err := net.DialUDP("udp4", nil, srv.backend)
	if err != nil {
		return nil, fmt.Errorf("voice backend of %s: %w", srv.uuid, err)
	}
	sess := &voiceSession{backend: backend}
	go func() {
		buf := make([]byte, 64*1024)
		out := make([]byte, 0, voicewire.DataOverhead+len(buf))
		for {
			n, err := backend.Read(buf)
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			out = voicewire.AppendData(out[:0], voicewire.TypeDataDown, k.server, k.session, buf[:n])
			_, _ = conn.WriteToUDPAddrPort(out, k.edge)
		}
	}()
	return sess, nil
}

func (f *voiceForwarder) closeSessionLocked(k voiceSessionKey, sess *voiceSession) {
	sess.backend.Close()
	delete(f.sessions, k)
}

// sweep closes sessions with no DATA_UP for voiceSessionIdle, the relay's own
// expiry for a player that went quiet.
func (f *voiceForwarder) sweep() {
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, sess := range f.sessions {
		if sess.active.Swap(false) {
			sess.lastSeen = now
		} else if now.Sub(sess.lastSeen) >= voiceSessionIdle {
			f.closeSessionLocked(k, sess)
		}
	}
}

func (f *voiceForwarder) shutdown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, sess := range f.sessions {
		f.closeSessionLocked(k, sess)
	}
	if f.conn != nil {
		f.conn.Close()
		f.conn = nil
	}
	f.servers = map[[16]byte]*voiceServer{}
}
