package servers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// portsFakeStore keeps server_ports in memory with the table's unique key.
type portsFakeStore struct {
	store.Store
	servers  map[int]*models.Server
	node     *models.Node
	settings map[string]string
	perms    map[string]bool
	limits   map[string]*int
	ranges   map[int]string
	hosts    map[int]string
	ports    []store.ServerPort
	nextID   int
}

func (f *portsFakeStore) GetServerByID(id int) (*models.Server, error) { return f.servers[id], nil }
func (f *portsFakeStore) GetNodeByID(int) (*models.Node, error)        { return f.node, nil }
func (f *portsFakeStore) GetSetting(k string) (string, error) {
	v, ok := f.settings[k]
	if !ok {
		return "", sql.ErrNoRows
	}
	return v, nil
}
func (f *portsFakeStore) GetUserBilling(string) (*store.UserBilling, error) {
	return nil, nil
}
func (f *portsFakeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}
func (f *portsFakeStore) GetPortPermission(scope string) (*bool, error) {
	if v, ok := f.perms[scope]; ok {
		return &v, nil
	}
	return nil, nil
}
func (f *portsFakeStore) GetPortLimit(scope string) (*store.PortLimit, error) {
	if v, ok := f.limits[scope]; ok {
		return &store.PortLimit{Scope: scope, MaxPorts: v}, nil
	}
	return nil, nil
}
func (f *portsFakeStore) GetNodeExtraPortRanges() (map[int]string, error) { return f.ranges, nil }
func (f *portsFakeStore) GetNodeExtraPortsHosts() (map[int]string, error) { return f.hosts, nil }
func (f *portsFakeStore) UpdateServerPortMeta(id int, name, note string) error {
	for i := range f.ports {
		if f.ports[i].ID == id {
			f.ports[i].Name, f.ports[i].Note = name, note
		}
	}
	return nil
}
func (f *portsFakeStore) UpdateServerPortPreset(id int, preset string, containerPort int) error {
	for i := range f.ports {
		if f.ports[i].ID == id {
			f.ports[i].Preset, f.ports[i].ContainerPort = preset, containerPort
		}
	}
	return nil
}
func (f *portsFakeStore) ListServerPorts(serverID int) ([]store.ServerPort, error) {
	out := []store.ServerPort{}
	for _, p := range f.ports {
		if p.ServerID == serverID {
			out = append(out, p)
		}
	}
	return out, nil
}
func (f *portsFakeStore) GetServerPort(id int) (*store.ServerPort, error) {
	for _, p := range f.ports {
		if p.ID == id {
			return &p, nil
		}
	}
	return nil, nil
}
func (f *portsFakeStore) CountServerPortsByOwner(owner string) (int, error) {
	n := 0
	for _, p := range f.ports {
		if s := f.servers[p.ServerID]; s != nil && s.OwnerID == owner {
			n++
		}
	}
	return n, nil
}
func (f *portsFakeStore) CreateServerPort(p store.ServerPort) (store.ServerPort, error) {
	for _, q := range f.ports {
		if q.NodeID == p.NodeID && q.Protocol == p.Protocol && q.HostPort == p.HostPort {
			return store.ServerPort{}, store.ErrPortTaken
		}
	}
	f.nextID++
	p.ID = f.nextID
	f.ports = append(f.ports, p)
	return p, nil
}
func (f *portsFakeStore) FreeServerPort(nodeID int, protocol string, min, max int, _ []int) (int, error) {
	for port := min; port <= max; port++ {
		taken := false
		for _, q := range f.ports {
			if q.NodeID == nodeID && q.Protocol == protocol && q.HostPort == port {
				taken = true
			}
		}
		if !taken {
			return port, nil
		}
	}
	return 0, store.ErrPortRangeFull
}
func (f *portsFakeStore) DeleteServerPort(id int) error {
	for i, p := range f.ports {
		if p.ID == id {
			f.ports = append(f.ports[:i], f.ports[i+1:]...)
			return nil
		}
	}
	return nil
}

type portsCase struct {
	fs  *portsFakeStore
	rdb *redis.Client
	mr  *miniredis.Miniredis
	h   *ServerHandler
}

// newPortsCase: server 1 (owner u1) and server 2 (owner u2) on node 7, direct
// routing, ports enabled globally, range 26000-26002, the node on a release
// that publishes extra ports and with game range 25600-25699.
func newPortsCase(t *testing.T) *portsCase {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	fs := &portsFakeStore{
		servers: map[int]*models.Server{
			1: {ID: 1, UUID: "srv-1", OwnerID: "u1", NodeID: 7, ActiveSubServer: "main"},
			2: {ID: 2, UUID: "srv-2", OwnerID: "u2", NodeID: 7, ActiveSubServer: "main"},
		},
		node: &models.Node{ID: 7, Token: "tok-7", Address: "203.0.113.7"},
		settings: map[string]string{
			"routing_mode":                     "ip_port",
			services.PortsEnabledSettingKey:    "true",
			services.PortsRangeSettingKey:      "26000-26002",
			services.PortsMaxPerUserSettingKey: "unlimited",
		},
	}
	setPortsHeartbeat(t, mr, services.PortsNodeSince, "25600-25699")
	return &portsCase{fs: fs, rdb: rdb, mr: mr, h: NewServerHandler(&handlers.AppState{Store: fs, Redis: rdb})}
}

func setPortsHeartbeat(t *testing.T, mr *miniredis.Miniredis, release, gameRange string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"releaseVersion": release, "portRange": gameRange})
	if err := mr.Set("dylaris:discovery:tok-7", string(b)); err != nil {
		t.Fatal(err)
	}
}

func (c *portsCase) do(method string, vars map[string]string, body string, admin bool, fn http.HandlerFunc) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/x", strings.NewReader(body))
	r = mux.SetURLVars(r, vars)
	r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "userID", "caller"), "isAdmin", admin))
	w := httptest.NewRecorder()
	fn(w, r)
	return w
}

func (c *portsCase) create(body string, admin bool) *httptest.ResponseRecorder {
	return c.do("POST", map[string]string{"id": "1"}, body, admin, c.h.CreateServerPort)
}

func errCode(w *httptest.ResponseRecorder) string {
	var out struct {
		Code string `json:"code"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	return out.Code
}

const tcpPort = `{"protocol":"tcp","containerPort":8100,"name":"Map"}`

func TestCreateServerPortRefusesWithAReasonForEachCase(t *testing.T) {
	zero := 0
	for _, c := range []struct {
		name   string
		setup  func(*portsCase)
		body   string
		admin  bool
		status int
		code   string
	}{
		{"gateway routing", func(c *portsCase) { c.fs.settings["routing_mode"] = "gateway" }, tcpPort, true, 409, "ports_gateway_mode"},
		{"not enabled for the server", func(c *portsCase) { c.fs.settings[services.PortsEnabledSettingKey] = "false" }, tcpPort, false, 403, "ports_not_allowed"},
		{"a scope row says no", func(c *portsCase) { c.fs.perms = map[string]bool{"server:1": false} }, tcpPort, false, 403, "ports_not_allowed"},
		{"a user names a host port", nil, `{"protocol":"tcp","containerPort":8100,"name":"Map","hostPort":26001}`, false, 403, "port_invalid"},
		{"limit of none", func(c *portsCase) { c.fs.limits = map[string]*int{"user:u1": &zero} }, tcpPort, false, 409, "ports_limit_reached"},
		{"no range", func(c *portsCase) { c.fs.settings[services.PortsRangeSettingKey] = "" }, tcpPort, false, 409, "ports_no_range"},
		{"range overlaps the game ports", func(c *portsCase) { c.fs.settings[services.PortsRangeSettingKey] = "25650-25700" }, tcpPort, false, 409, "ports_range_conflict"},
		{"node too old", func(c *portsCase) { setPortsHeartbeat(t, c.mr, "2026.10.24", "25600-25699") }, tcpPort, false, 409, "node_too_old"},
		{"no name", nil, `{"protocol":"tcp","containerPort":8100}`, false, 400, "port_invalid"},
		{"bad protocol", nil, `{"protocol":"sctp","containerPort":8100,"name":"x"}`, false, 400, "port_invalid"},
		{"admin port in the game range", nil, `{"protocol":"tcp","containerPort":8100,"name":"Map","hostPort":25610}`, true, 400, "port_out_of_range"},
		{"admin port on the node's own ports", nil, `{"protocol":"tcp","containerPort":8100,"name":"Map","hostPort":25521}`, true, 400, "port_out_of_range"},
		{"voice preset with the gateway on", func(c *portsCase) { c.fs.settings["routing_mode"] = "both" }, `{"preset":"svc"}`, false, 409, "voice_preset_mode"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pc := newPortsCase(t)
			if c.setup != nil {
				c.setup(pc)
			}
			w := pc.create(c.body, c.admin)
			if w.Code != c.status || errCode(w) != c.code {
				t.Fatalf("status %d code %q (%s), want %d %q", w.Code, errCode(w), w.Body.String(), c.status, c.code)
			}
			if len(pc.fs.ports) != 0 {
				t.Fatalf("a refused request took a port: %+v", pc.fs.ports)
			}
		})
	}
}

func TestCreateServerPortTakesTheLowestFreePortAndPublishesIt(t *testing.T) {
	pc := newPortsCase(t)
	pc.fs.ports = []store.ServerPort{{ID: 90, ServerID: 2, NodeID: 7, Protocol: "tcp", HostPort: 26000, ContainerPort: 1}}
	pc.fs.nextID = 90
	w := pc.create(tcpPort, false)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Port            store.ServerPort `json:"port"`
		RestartRequired bool             `json:"restartRequired"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Port.HostPort != 26001 || !out.RestartRequired {
		t.Fatalf("got %+v restart=%v, want 26001 (26000 is server 2's) and a restart", out.Port, out.RestartRequired)
	}
	got, _ := pc.mr.Get("dylaris:server:srv-1:ports")
	if got != `{"v":1,"ports":[{"p":"tcp","h":26001,"c":8100}]}` {
		t.Fatalf("node key = %s", got)
	}
	// UDP has its own numbers: 26000 is free there.
	w = pc.create(`{"protocol":"udp","containerPort":8100,"name":"Query"}`, false)
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.Port.HostPort != 26000 {
		t.Fatalf("udp: status %d port %+v", w.Code, out.Port)
	}
}

func TestCreateServerPortSaysWhenTheRangeIsFull(t *testing.T) {
	pc := newPortsCase(t)
	for _, port := range []int{26000, 26001, 26002} {
		pc.fs.ports = append(pc.fs.ports, store.ServerPort{ID: port, ServerID: 2, NodeID: 7, Protocol: "tcp", HostPort: port, ContainerPort: port})
	}
	pc.fs.nextID = 30000
	w := pc.create(tcpPort, false)
	if w.Code != 409 || errCode(w) != "ports_range_full" {
		t.Fatalf("status %d code %q", w.Code, errCode(w))
	}
}

// The limit counts every port the owner holds, and an admin is not refused
// by it (they set it).
func TestCreateServerPortCountsTheOwnersPorts(t *testing.T) {
	one := 1
	pc := newPortsCase(t)
	pc.fs.limits = map[string]*int{"user:u1": &one}
	if w := pc.create(tcpPort, false); w.Code != 200 {
		t.Fatalf("first port: %d %s", w.Code, w.Body.String())
	}
	if w := pc.create(`{"protocol":"udp","containerPort":9,"name":"x"}`, false); w.Code != 409 || errCode(w) != "ports_limit_reached" {
		t.Fatalf("second port: %d %q", w.Code, errCode(w))
	}
	if w := pc.create(`{"protocol":"udp","containerPort":9,"name":"x","hostPort":30000}`, true); w.Code != 200 {
		t.Fatalf("admin: %d %s", w.Code, w.Body.String())
	}
	if w := pc.create(`{"protocol":"tcp","containerPort":10,"name":"y","hostPort":26000}`, true); w.Code != 409 || errCode(w) != "port_taken" {
		t.Fatalf("admin on a taken port: %d %q", w.Code, errCode(w))
	}
}

// The route's access check is on {id}; a port id of ANOTHER server must not
// be reachable through it.
func TestAPortIsOnlyReachableThroughItsOwnServer(t *testing.T) {
	pc := newPortsCase(t)
	pc.fs.ports = []store.ServerPort{{ID: 5, ServerID: 2, NodeID: 7, Protocol: "udp", HostPort: 26000, ContainerPort: 1, Name: "theirs"}}
	w := pc.do("DELETE", map[string]string{"id": "1", "portId": "5"}, "", false, pc.h.DeleteServerPort)
	if w.Code != 404 || len(pc.fs.ports) != 1 {
		t.Fatalf("status %d, ports %+v: server 1 removed server 2's port", w.Code, pc.fs.ports)
	}
	w = pc.do("PATCH", map[string]string{"id": "1", "portId": "5"}, `{"name":"mine"}`, false, pc.h.UpdateServerPort)
	if w.Code != 404 {
		t.Fatalf("PATCH status %d", w.Code)
	}
}

// Taking a port away works in every mode, and the node key goes with it.
func TestRemovingAPortWorksBehindTheGatewayToo(t *testing.T) {
	pc := newPortsCase(t)
	if w := pc.create(tcpPort, false); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	pc.fs.settings["routing_mode"] = "gateway"
	pc.fs.settings[services.PortsEnabledSettingKey] = "false"
	w := pc.do("DELETE", map[string]string{"id": "1", "portId": "1"}, "", false, pc.h.DeleteServerPort)
	if w.Code != 200 || len(pc.fs.ports) != 0 {
		t.Fatalf("status %d ports %+v", w.Code, pc.fs.ports)
	}
	if pc.mr.Exists("dylaris:server:srv-1:ports") {
		t.Fatal("the node key outlived the last port")
	}
}

// portsAuthzStore puts the real resolver in front of the ports endpoints: the
// server and grant answers come from serverPowerFakeStore, the port tables
// are empty.
type portsAuthzStore struct {
	*serverPowerFakeStore
}

func (f portsAuthzStore) GetNodeExtraPortRanges() (map[int]string, error) { return nil, nil }
func (f portsAuthzStore) GetNodeExtraPortsHosts() (map[int]string, error) { return nil, nil }
func (f portsAuthzStore) CountServerPortsByOwner(string) (int, error)     { return 0, nil }
func (f portsAuthzStore) GetPortPermission(string) (*bool, error)         { return nil, nil }
func (f portsAuthzStore) GetPortLimit(string) (*store.PortLimit, error)   { return nil, nil }

// The panel offers add/edit/remove to whoever the write routes accept: a
// member with network.write, not only the owner or an admin.
func TestGetServerPortsSaysWhetherTheCallerMayWrite(t *testing.T) {
	roleID := 9
	for _, c := range []struct {
		name   string
		caller string
		caps   []string
		want   bool
	}{
		{"owner", "u1", nil, true},
		{"member with network.write", "u3", []string{"network.read", "network.write"}, true},
		{"member with network.read only", "u3", []string{"network.read"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := &serverPowerFakeStore{
				server:   &models.Server{ID: 1, UUID: "srv-1", OwnerID: "u1", OwnerName: "alice", NodeID: 7},
				node:     &models.Node{ID: 7, Token: "tok-7"},
				settings: map[string]string{"routing_mode": "ip_port"},
			}
			if c.caps != nil {
				fs.serverGrant = &store.ServerGrant{UserID: c.caller, ServerRoleID: &roleID}
				fs.serverRole = &store.ServerRole{ID: roleID, Capabilities: c.caps}
			}
			st := portsAuthzStore{fs}
			h := NewServerHandler(&handlers.AppState{Store: st, Authz: authz.NewResolver(st)})
			r := httptest.NewRequest("GET", "/x", nil)
			r = mux.SetURLVars(r, map[string]string{"id": "1"})
			r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "userID", c.caller), "isAdmin", false))
			w := httptest.NewRecorder()
			h.GetServerPorts(w, r)
			var out struct {
				Ports struct {
					CanWrite bool `json:"canWrite"`
				} `json:"ports"`
			}
			json.Unmarshal(w.Body.Bytes(), &out)
			if w.Code != 200 || out.Ports.CanWrite != c.want {
				t.Fatalf("status %d canWrite %v, want %v: %s", w.Code, out.Ports.CanWrite, c.want, w.Body.String())
			}
		})
	}
}

// The overview carries each listed server's traffic as its node counted it,
// and nothing for a server the node has not reported (an old node).
func TestPortsOverviewCarriesServerTraffic(t *testing.T) {
	pc := newPortsCase(t)
	pc.fs.ports = []store.ServerPort{{ID: 1, ServerID: 1, NodeID: 7, Protocol: "udp", HostPort: 26000, ContainerPort: 24454, Name: "Voice"}}
	pc.mr.HSet("dylaris:server:srv-1:net", "rx", "2048", "tx", "1024", "rx_bps", "300", "tx_bps", "100", "ts", "1700000000")
	st := portsOverviewStore{pc.fs}
	h := NewPortsAdminHandler(&handlers.AppState{Store: st, Redis: pc.rdb})
	w := httptest.NewRecorder()
	h.GetOverview(w, httptest.NewRequest("GET", "/x", nil))
	var out struct {
		Traffic map[string]struct {
			Rx, Tx, RxBps, TxBps uint64
		} `json:"traffic"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := out.Traffic["1"]; got.Rx != 2048 || got.Tx != 1024 || got.RxBps != 300 || got.TxBps != 100 {
		t.Fatalf("traffic = %+v (%s)", out.Traffic, w.Body.String())
	}
	if _, ok := out.Traffic["2"]; ok {
		t.Fatal("a server without a report got numbers")
	}
}

// portsOverviewStore adds what the overview reads beyond the server ports.
type portsOverviewStore struct{ *portsFakeStore }

func (f portsOverviewStore) ListAllServerPorts(string) ([]store.ServerPortRow, error) {
	out := []store.ServerPortRow{}
	for _, p := range f.ports {
		out = append(out, store.ServerPortRow{ServerPort: p, ServerUUID: f.servers[p.ServerID].UUID})
	}
	return out, nil
}
func (f portsOverviewStore) ListNodes() ([]models.Node, error) { return []models.Node{*f.node}, nil }

// A node without its own range and a global range never saved gets the
// default range when self-hosted, and none on the SaaS. A saved global range,
// even an empty one, wins over the default.
func TestCreateServerPortUsesTheDefaultRangeOnlyWhenSelfHosted(t *testing.T) {
	for _, c := range []struct {
		name     string
		hosted   bool
		global   *string
		wantPort int
		wantCode string
	}{
		{"self-hosted, nothing saved", false, nil, 25700, ""},
		{"hosted, nothing saved", true, nil, 0, "ports_no_range"},
		{"self-hosted, saved empty", false, ptr(""), 0, "ports_no_range"},
		{"self-hosted, saved range", false, ptr("26000-26002"), 26000, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			pc := newPortsCase(t)
			pc.h.state.Hosted = c.hosted
			delete(pc.fs.settings, services.PortsRangeSettingKey)
			if c.global != nil {
				pc.fs.settings[services.PortsRangeSettingKey] = *c.global
			}
			w := pc.create(tcpPort, false)
			if c.wantCode != "" {
				if errCode(w) != c.wantCode {
					t.Fatalf("status %d code %q, want %q", w.Code, errCode(w), c.wantCode)
				}
				return
			}
			if w.Code != 200 || len(pc.fs.ports) != 1 || pc.fs.ports[0].HostPort != c.wantPort {
				t.Fatalf("status %d ports %+v, want host port %d", w.Code, pc.fs.ports, c.wantPort)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestNormPortsHost(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"  ", "", true},
		{"198.51.100.4", "198.51.100.4", true},
		{" Voice.Example.COM. ", "voice.example.com", true},
		{"2001:db8::1", "", false},
		{"localhost", "", false},
		{"1.2.3", "", false},
		{"bad_name.example.com", "", false},
		{"http://example.com", "", false},
		{"example.com:24454", "", false},
	} {
		got, err := normPortsHost(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("normPortsHost(%q) = %q, %v; want %q ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

// The address an admin set for the node's extra ports is what the server's
// port view shows and what the voice preset writes on every start; without
// one, the node's public address is.
func TestExtraPortsAddressOverridesThePublicIP(t *testing.T) {
	for _, c := range []struct {
		name  string
		hosts map[int]string
		want  string
	}{
		{"automatic", nil, "203.0.113.7"},
		{"override", map[int]string{7: "voice.example.com"}, "voice.example.com"},
		{"another node's override", map[int]string{8: "voice.example.com"}, "203.0.113.7"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pc := newPortsCase(t)
			pc.fs.hosts = c.hosts
			pc.fs.ports = []store.ServerPort{{ID: 1, ServerID: 1, NodeID: 7, Protocol: "udp", HostPort: 26000, ContainerPort: voicePresetPort, Name: "Voice chat", Preset: "svc"}}
			st, _, node := voiceNodeState(t, pc.fs, "config/voicechat/voicechat-server.properties", "svc_before.properties")
			st.Hosted = false
			h := NewServerHandler(st)

			w := pc.do("GET", map[string]string{"id": "1"}, "", true, h.GetServerPorts)
			var out struct {
				Ports struct {
					Address string `json:"address"`
				} `json:"ports"`
			}
			json.Unmarshal(w.Body.Bytes(), &out)
			if w.Code != 200 || out.Ports.Address != c.want {
				t.Fatalf("view address %q (%d), want %q", out.Ports.Address, w.Code, c.want)
			}

			h.syncPortsToStart(pc.fs.servers[1], "main")
			node.mu.Lock()
			written := node.written.String()
			node.mu.Unlock()
			if !strings.Contains(written, "voice_host="+c.want+":26000") {
				t.Fatalf("start wrote %q, want voice_host=%s:26000", written, c.want)
			}
		})
	}
}

// PATCH may change the preset: a plain UDP port becomes the voice port (the
// mod's config is written and the container port follows the mod), the voice
// port becomes a plain one, and a TCP port is refused.
func TestUpdateServerPortChangesThePreset(t *testing.T) {
	t.Run("plain udp to voice", func(t *testing.T) {
		pc := newPortsCase(t)
		pc.fs.hosts = map[int]string{7: "voice.example.com"}
		pc.fs.ports = []store.ServerPort{{ID: 1, ServerID: 1, NodeID: 7, Protocol: "udp", HostPort: 26000, ContainerPort: 9000, Name: "Voice"}}
		st, _, node := voiceNodeState(t, pc.fs, "config/voicechat/voicechat-server.properties", "svc_before.properties")
		h := NewServerHandler(st)
		w := pc.do("PATCH", map[string]string{"id": "1", "portId": "1"}, `{"name":"Voice","note":"","preset":"svc"}`, false, h.UpdateServerPort)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"restartRequired":true`) {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		if p := pc.fs.ports[0]; p.Preset != "svc" || p.ContainerPort != voicePresetPort {
			t.Fatalf("port %+v, want preset svc on container port %d", p, voicePresetPort)
		}
		node.mu.Lock()
		written := node.written.String()
		node.mu.Unlock()
		if !strings.Contains(written, "voice_host=voice.example.com:26000") {
			t.Fatalf("wrote %q", written)
		}
	})
	t.Run("voice to plain", func(t *testing.T) {
		pc := newPortsCase(t)
		pc.fs.ports = []store.ServerPort{{ID: 1, ServerID: 1, NodeID: 7, Protocol: "udp", HostPort: 26000, ContainerPort: voicePresetPort, Name: "Voice", Preset: "svc"}}
		st, _, _ := voiceNodeState(t, pc.fs, "config/voicechat/voicechat-server.properties", "svc_after.properties")
		h := NewServerHandler(st)
		w := pc.do("PATCH", map[string]string{"id": "1", "portId": "1"}, `{"name":"Voice","note":"","preset":""}`, false, h.UpdateServerPort)
		if w.Code != 200 || pc.fs.ports[0].Preset != "" {
			t.Fatalf("status %d port %+v: %s", w.Code, pc.fs.ports[0], w.Body.String())
		}
	})
	t.Run("tcp refused", func(t *testing.T) {
		pc := newPortsCase(t)
		pc.fs.ports = []store.ServerPort{{ID: 1, ServerID: 1, NodeID: 7, Protocol: "tcp", HostPort: 26000, ContainerPort: 8100, Name: "Map"}}
		w := pc.do("PATCH", map[string]string{"id": "1", "portId": "1"}, `{"name":"Map","note":"","preset":"svc"}`, false, pc.h.UpdateServerPort)
		if w.Code != 400 || errCode(w) != "port_invalid" || pc.fs.ports[0].Preset != "" {
			t.Fatalf("status %d code %q port %+v", w.Code, errCode(w), pc.fs.ports[0])
		}
	})
	t.Run("name only leaves the preset", func(t *testing.T) {
		pc := newPortsCase(t)
		pc.fs.ports = []store.ServerPort{{ID: 1, ServerID: 1, NodeID: 7, Protocol: "udp", HostPort: 26000, ContainerPort: voicePresetPort, Name: "Voice", Preset: "svc"}}
		w := pc.do("PATCH", map[string]string{"id": "1", "portId": "1"}, `{"name":"Talk","note":""}`, false, pc.h.UpdateServerPort)
		if w.Code != 200 || pc.fs.ports[0].Preset != "svc" || pc.fs.ports[0].Name != "Talk" {
			t.Fatalf("status %d port %+v", w.Code, pc.fs.ports[0])
		}
	})
}

// The overview shows a node without its own range on the default, says so,
// and carries the node's address for extra ports; on the SaaS it shows none.
func TestPortsOverviewShowsTheDefaultRange(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		pc := newPortsCase(t)
		delete(pc.fs.settings, services.PortsRangeSettingKey)
		pc.fs.hosts = map[int]string{7: "voice.example.com"}
		h := NewPortsAdminHandler(&handlers.AppState{Store: portsOverviewStore{pc.fs}, Redis: pc.rdb, Hosted: hosted})
		w := httptest.NewRecorder()
		h.GetOverview(w, httptest.NewRequest("GET", "/x", nil))
		var out struct {
			Nodes    []portsNodeUsage  `json:"nodes"`
			Settings portsSettingsView `json:"settings"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || len(out.Nodes) != 1 {
			t.Fatalf("hosted=%v: status %d: %s", hosted, w.Code, w.Body.String())
		}
		n := out.Nodes[0]
		want := services.DefaultExtraPortRange
		if hosted {
			want = ""
		}
		if n.Range != want || n.DefaultRange != !hosted || out.Settings.DefaultRange != want || n.Host != "voice.example.com" {
			t.Fatalf("hosted=%v: node %+v settings %+v", hosted, n, out.Settings)
		}
	}
}
