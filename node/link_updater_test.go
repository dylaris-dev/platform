package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

func TestPlanLinkUpdate(t *testing.T) {
	run := func(name string) linkCtr { return linkCtr{ID: name + "-id", Name: name, Running: true} }
	dead := func(name string) linkCtr { return linkCtr{ID: name + "-id", Name: name} }
	for _, tc := range []struct {
		name  string
		cs    []linkCtr
		want  linkStep
		clone string
		old   string
	}{
		{"none", nil, linkSkip, "", ""},
		{"only exited", []linkCtr{dead("l")}, linkSkip, "", ""},
		{"one running", []linkCtr{run("l"), dead("stray")}, linkIdle, "", ""},
		{"two running", []linkCtr{run("a"), run("b")}, linkSkip, "", ""},
		{"clone up, old not drained", []linkCtr{run("l"), run("l-next")}, linkCheckClone, "l-next", "l"},
		{"clone up, old draining", []linkCtr{run("l-draining"), run("l-next")}, linkWait, "", "l-draining"},
		{"clone up, old drained", []linkCtr{dead("l-draining"), run("l-next")}, linkFinish, "l-next", "l-draining"},
		{"clone up, old exited undrained", []linkCtr{dead("l"), run("l-next")}, linkFinish, "l-next", "l"},
		{"clone up, old gone", []linkCtr{run("l-next")}, linkFinish, "l-next", ""},
		{"clone dead, old serving", []linkCtr{run("l"), dead("l-next")}, linkRemoveClone, "l-next", "l"},
		{"clone dead while old drains", []linkCtr{run("l-draining"), dead("l-next")}, linkSkip, "", ""},
		{"draining, no clone", []linkCtr{run("l-draining"), run("l")}, linkWait, "", "l-draining"},
		{"drained, no clone", []linkCtr{dead("l-draining"), run("l")}, linkRemoveDrained, "", "l-draining"},
		{"mismatched pair", []linkCtr{run("a-draining"), run("b-next")}, linkSkip, "", ""},
		{"other link beside update", []linkCtr{run("l"), run("l-next"), run("x")}, linkSkip, "", ""},
		{"two clones", []linkCtr{run("a-next"), run("b-next")}, linkSkip, "", ""},
		{"abandoned clone blocks an update", []linkCtr{run("l"), run("l-abandoned")}, linkSkip, "", ""},
		{"old and drained both", []linkCtr{dead("l"), run("l-draining"), run("l-next")}, linkSkip, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := planLinkUpdate(tc.cs)
			if p.step != tc.want {
				t.Fatalf("step = %d (%s), want %d", p.step, p.reason, tc.want)
			}
			if tc.clone != "" && (p.clone == nil || p.clone.Name != tc.clone) {
				t.Errorf("clone = %+v, want %s", p.clone, tc.clone)
			}
			if tc.old != "" && (p.old == nil || p.old.Name != tc.old) {
				t.Errorf("old = %+v, want %s", p.old, tc.old)
			}
			if p.step == linkFinish && p.base != "l" {
				t.Errorf("base = %q, want l", p.base)
			}
		})
	}
}

func TestJudgeClone(t *testing.T) {
	start := time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		serving bool
		after   time.Duration
		want    cloneVerdict
	}{
		{true, time.Second, cloneDrainOld},
		{true, time.Hour, cloneDrainOld},
		{false, 2 * time.Minute, cloneWait},
		{false, 3 * time.Minute, cloneAbort},
	} {
		if got := judgeClone(tc.serving, start, start.Add(tc.after)); got != tc.want {
			t.Errorf("judgeClone(%v, +%s) = %d, want %d", tc.serving, tc.after, got, tc.want)
		}
	}
}

func TestLinkUpdateSchedule(t *testing.T) {
	for _, tc := range []struct {
		in   string
		h, m int
		ok   bool
	}{{"04:00", 4, 0, true}, {" 23:59 ", 23, 59, true}, {"24:00", 0, 0, false}, {"4am", 0, 0, false}, {"", 0, 0, false}} {
		h, m, err := parseLinkUpdateTime(tc.in)
		if (err == nil) != tc.ok || h != tc.h || m != tc.m {
			t.Errorf("parseLinkUpdateTime(%q) = %d:%d, %v", tc.in, h, m, err)
		}
	}
	loc := time.FixedZone("X", 2*3600)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 10, h, m, 0, 0, loc) }
	for _, tc := range []struct {
		now     time.Time
		lastRun string
		want    bool
	}{
		{at(3, 59), "", false},
		{at(4, 0), "", true},
		{at(4, 0), "2026-10-09", true},
		{at(4, 1), "2026-10-10", false},   // already ran today
		{at(10, 0), "", true},             // started after the time: runs once
		{at(23, 59), "2026-10-10", false}, // and not again
	} {
		if got := linkUpdateDue(tc.now, 4, 0, tc.lastRun); got != tc.want {
			t.Errorf("linkUpdateDue(%s, last %q) = %v, want %v", tc.now.Format("15:04"), tc.lastRun, got, tc.want)
		}
	}
}

func TestBuildLinkClone(t *testing.T) {
	id := "0123456789abcdef0123"
	info := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:   id,
			Name: "/byon-link-1",
			HostConfig: &container.HostConfig{
				RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
				Binds:         []string{"byon_link_data:/data"},
				NetworkMode:   "byon_dylaris_net",
			},
		},
		Config: &container.Config{
			Hostname:    "0123456789ab",
			Image:       "reg/gateway-link:latest",
			Env:         []string{"PATH=/old", "CORE_URL=https://core", "LINK_DRAIN_TIMEOUT=6h"},
			Cmd:         []string{"/app/binary"},
			Entrypoint:  []string{"/entry"},
			Healthcheck: &container.HealthConfig{Test: []string{"CMD", "probe"}},
			Labels: map[string]string{
				"com.docker.compose.project": "byon", "dylaris.component": "link",
				"com.dylaris.role": "link",
			},
		},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"byon_dylaris_net": {Aliases: []string{"byon-link-1", "link", "0123456789ab"}, IPAddress: "172.18.0.5", MacAddress: "02:42:ac:12:00:05",
				IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: "172.18.0.5"}},
		}},
	}
	img := imageDefaults{Env: []string{"PATH=/old"}, Cmd: []string{"/app/binary"}, Healthcheck: []string{"CMD", "probe"},
		Labels: map[string]string{"com.dylaris.role": "link"}}
	cfg, hc, nc, name := buildLinkClone(info, img)

	if name != "byon-link-1-next" {
		t.Errorf("name = %q", name)
	}
	if cfg.Image != "reg/gateway-link:latest" {
		t.Errorf("image = %q, want the reference", cfg.Image)
	}
	if strings.Join(cfg.Env, ",") != "CORE_URL=https://core,LINK_DRAIN_TIMEOUT=6h" {
		t.Errorf("env = %v, want the image's PATH dropped and the rest kept", cfg.Env)
	}
	if cfg.Cmd != nil || cfg.Healthcheck != nil {
		t.Errorf("image-default cmd/healthcheck copied: %v %v", cfg.Cmd, cfg.Healthcheck)
	}
	if len(cfg.Entrypoint) != 1 || cfg.Entrypoint[0] != "/entry" {
		t.Errorf("own entrypoint lost: %v", cfg.Entrypoint)
	}
	if cfg.Labels["com.docker.compose.project"] != "byon" || cfg.Labels["dylaris.component"] != "link" || cfg.Labels["com.dylaris.role"] != "" {
		t.Errorf("labels = %v", cfg.Labels)
	}
	if cfg.Hostname != "" {
		t.Errorf("hostname = %q, want the old short id dropped", cfg.Hostname)
	}
	if hc.RestartPolicy.Name != container.RestartPolicyUnlessStopped || hc.NetworkMode != "byon_dylaris_net" || len(hc.Binds) != 1 {
		t.Errorf("host config not carried: %+v", hc)
	}
	ep := nc.EndpointsConfig["byon_dylaris_net"]
	if ep == nil || strings.Join(ep.Aliases, ",") != "byon-link-1,link" || ep.IPAMConfig != nil || ep.MacAddress != "" || ep.IPAddress != "" {
		t.Errorf("endpoint = %+v", ep)
	}
	if info.Config.Labels["com.dylaris.role"] != "link" || len(info.Config.Env) != 3 {
		t.Error("buildLinkClone mutated the inspected config")
	}
}

func TestTunnelsUp(t *testing.T) {
	up := func(a string) string {
		return "2026/10/10 04:00:00 [Link] Secure Tunnel established to " + a + " over the internet!\n"
	}
	lost := func(a string) string { return "2026/10/10 04:00:00 [Link] Connection to " + a + " lost: EOF\n" }
	for _, tc := range []struct {
		name, log string
		want      int
	}{
		{"none", "booting\n", 0},
		{"one", up("1.1.1.1:7000"), 1},
		{"two edges", up("1.1.1.1:7000") + up("2.2.2.2:7000"), 2},
		{"reconnect counted once", up("1.1.1.1:7000") + lost("1.1.1.1:7000") + up("1.1.1.1:7000"), 1},
		{"edge gone", up("1.1.1.1:7000") + up("2.2.2.2:7000") + lost("2.2.2.2:7000"), 1},
		{"beam tunnel ignored", "[Link/Beam] Tunnel established to Beam Relay 3.3.3.3:9000\n", 0},
	} {
		if got := tunnelsUp(tc.log); got != tc.want {
			t.Errorf("%s: tunnelsUp = %d, want %d", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		clone, old int
		want       bool
	}{{0, 0, false}, {1, 0, true}, {1, 2, false}, {2, 2, true}, {3, 2, true}} {
		if got := cloneReady(tc.clone, tc.old); got != tc.want {
			t.Errorf("cloneReady(%d, %d) = %v", tc.clone, tc.old, got)
		}
	}
}

// fakeLinkDocker is a stateful Docker API for the Link containers. A Link it
// signals logs "Received signal" like the real one.
type fakeLinkDocker struct {
	mu    sync.Mutex
	ctrs  map[string]*fakeCtr // by id
	calls []string
}

type fakeCtr struct {
	name, image string
	running     bool
	labels      map[string]string
	env         []string // nil: the default env every fake Link reports
	created     time.Time
	started     time.Time // zero: same as created
	log         string
}

func (f *fakeLinkDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	if i := strings.Index(p[1:], "/"); strings.HasPrefix(p, "/v") && i > 0 {
		p = p[i+1:]
	}
	w.Header().Set("Content-Type", "application/json")
	ctrID := func() string {
		s := strings.TrimPrefix(p, "/containers/")
		s, _, _ = strings.Cut(s, "/")
		return s
	}
	switch {
	case r.Method == http.MethodGet && p == "/containers/json":
		var out []map[string]any
		var flt struct{ Label map[string]bool }
		json.Unmarshal([]byte(r.URL.Query().Get("filters")), &flt)
	ctrs:
		for id, c := range f.ctrs {
			for l := range flt.Label {
				k, v, _ := strings.Cut(l, "=")
				if c.labels[k] != v {
					continue ctrs
				}
			}
			state := "exited"
			if c.running {
				state = "running"
			}
			out = append(out, map[string]any{"Id": id, "Names": []string{"/" + c.name}, "ImageID": c.image, "State": state, "Labels": c.labels})
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost && p == "/images/create":
		f.calls = append(f.calls, "pull "+r.URL.Query().Get("fromImage")+":"+r.URL.Query().Get("tag"))
		w.Write([]byte(`{"status":"done"}`))
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/images/"):
		f.calls = append(f.calls, "rmi "+strings.TrimPrefix(p, "/images/"))
		w.Write([]byte(`[]`))
	case strings.HasPrefix(p, "/images/"):
		ref := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json")
		switch ref {
		case "reg/gateway-link:latest":
			w.Write([]byte(`{"Id":"sha256:new","Config":{"Env":["PATH=/new"]}}`))
		case "sha256:new":
			w.Write([]byte(`{"Id":"sha256:new","Config":{"Env":["PATH=/new"]}}`))
		case "sha256:old":
			w.Write([]byte(`{"Id":"sha256:old","Config":{"Env":["PATH=/old"]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodPost && p == "/containers/create":
		var body struct {
			Labels map[string]string
			Env    []string
		}
		json.NewDecoder(r.Body).Decode(&body)
		name := r.URL.Query().Get("name")
		f.calls = append(f.calls, "create "+name+" "+strings.Join(body.Env, ","))
		f.ctrs["clone-id"] = &fakeCtr{name: name, image: "sha256:new", labels: body.Labels, env: body.Env, created: updateAt}
		w.Write([]byte(`{"Id":"clone-id"}`))
	default:
		id := ctrID()
		c := f.ctrs[id]
		if c == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"no such container"}`))
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/json"):
			env := c.env
			if env == nil {
				env = []string{"PATH=/old", "CORE_URL=x"}
			}
			started := c.started
			if started.IsZero() {
				started = c.created
			}
			json.NewEncoder(w).Encode(map[string]any{
				"Id": id, "Name": "/" + c.name, "Image": c.image, "Created": c.created.UTC().Format(time.RFC3339Nano),
				"State":      map[string]any{"Running": c.running, "StartedAt": started.UTC().Format(time.RFC3339Nano)},
				"Config":     map[string]any{"Image": "reg/gateway-link:latest", "Env": env, "Labels": c.labels},
				"HostConfig": map[string]any{"RestartPolicy": map[string]any{"Name": "unless-stopped"}},
			})
			return
		case strings.HasSuffix(p, "/logs"):
			io.WriteString(w, c.log)
			return
		case strings.HasSuffix(p, "/start"):
			c.running = true
			f.calls = append(f.calls, "start "+c.name)
		case strings.HasSuffix(p, "/rename"):
			f.calls = append(f.calls, "rename "+c.name+" "+r.URL.Query().Get("name"))
			c.name = r.URL.Query().Get("name")
		case strings.HasSuffix(p, "/update"):
			var u container.UpdateConfig
			json.NewDecoder(r.Body).Decode(&u)
			f.calls = append(f.calls, "update "+c.name+" restart="+string(u.RestartPolicy.Name))
			w.Write([]byte(`{"Warnings":[]}`))
			return
		case strings.HasSuffix(p, "/kill"):
			f.calls = append(f.calls, "kill "+c.name+" "+r.URL.Query().Get("signal"))
			c.log += "Received signal terminated, draining for up to 6h0m0s before shutdown...\n"
		case r.Method == http.MethodDelete:
			f.calls = append(f.calls, "remove "+c.name)
			delete(f.ctrs, id)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeLinkDocker) set(id string, edit func(*fakeCtr)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f.ctrs[id])
}

func newLinkUpdaterTest(t *testing.T, ctrs map[string]*fakeCtr) (*linkUpdater, *fakeLinkDocker, func(step string, want ...string)) {
	t.Helper()
	f := &fakeLinkDocker{ctrs: ctrs}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	expect := func(step string, want ...string) {
		t.Helper()
		f.mu.Lock()
		got := f.calls
		f.calls = nil
		f.mu.Unlock()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: calls\n got %q\nwant %q", step, got, want)
		}
	}
	return &linkUpdater{cli: cli, hour: 4}, f, expect
}

var (
	testLinkLabels = map[string]string{"dylaris.component": "link"}
	edgeA          = "[Link] Secure Tunnel established to 1.1.1.1:7000 over the internet!\n"
	edgeB          = "[Link] Secure Tunnel established to 2.2.2.2:7000 over the internet!\n"
	updateAt       = time.Date(2026, 10, 10, 4, 0, 0, 0, time.Local)
)

// One update end to end, as the ticks see it: clone started, old drained only
// once the clone holds as many tunnels as the old one, the clone renamed only
// once the old one exits, and the old image removed.
func TestLinkUpdaterHappyPath(t *testing.T) {
	u, f, expect := newLinkUpdaterTest(t, map[string]*fakeCtr{
		"old-id": {name: "byon-link-1", image: "sha256:old", running: true, labels: testLinkLabels, log: edgeA + edgeB},
	})
	ctx := t.Context()
	now := updateAt.Add(time.Minute)

	u.tick(ctx, updateAt)
	expect("update", "pull docker.io/reg/gateway-link:latest", "create byon-link-1-next CORE_URL=x", "start byon-link-1-next")

	u.tick(ctx, now)
	expect("no tunnel yet: wait")

	f.set("clone-id", func(c *fakeCtr) { c.log = edgeA })
	u.tick(ctx, now)
	expect("one of two edges: wait")

	f.set("clone-id", func(c *fakeCtr) { c.log = edgeA + edgeB })
	u.tick(ctx, now)
	expect("drain",
		"rename byon-link-1 byon-link-1-draining",
		"update byon-link-1-draining restart=no",
		"kill byon-link-1-draining SIGTERM")

	u.tick(ctx, now)
	expect("still draining: no second signal")

	f.set("old-id", func(c *fakeCtr) { c.running = false })
	u.tick(ctx, now)
	expect("finish", "remove byon-link-1-draining", "rename byon-link-1-next byon-link-1", "rmi sha256:old")

	u.tick(ctx, updateAt.Add(time.Hour))
	expect("idle, already ran today")
}

// A node stopped between the rename and the signal: the draining Link has no
// "Received signal" line, so it gets its one SIGTERM now, and only once.
func TestLinkUpdaterSignalsAnInterruptedDrain(t *testing.T) {
	u, _, expect := newLinkUpdaterTest(t, map[string]*fakeCtr{
		"old-id":   {name: "l-draining", image: "sha256:old", running: true, labels: testLinkLabels, log: edgeA},
		"clone-id": {name: "l-next", image: "sha256:new", running: true, labels: testLinkLabels, log: edgeA, created: updateAt},
	})
	u.tick(t.Context(), updateAt)
	expect("resume", "update l-draining restart=no", "kill l-draining SIGTERM")
	u.tick(t.Context(), updateAt)
	expect("signalled already")
}

// A clone that never gets its tunnels (timed from its creation, not from its
// last restart) is not removed (it may carry players):
// it drains as "-abandoned", blocks the next update while it does, and is
// removed once it exits. The old Link is never touched.
func TestLinkUpdaterAbandonsASlowClone(t *testing.T) {
	u, f, expect := newLinkUpdaterTest(t, map[string]*fakeCtr{
		"old-id":   {name: "l", image: "sha256:old", running: true, labels: testLinkLabels, log: edgeA + edgeB},
		"clone-id": {name: "l-next", image: "sha256:new", running: true, labels: testLinkLabels, log: edgeA, created: updateAt.Add(-linkServeTimeout), started: updateAt},
	})
	u.tick(t.Context(), updateAt)
	expect("abandon", "rename l-next l-abandoned", "update l-abandoned restart=no", "kill l-abandoned SIGTERM")

	nextDay := updateAt.AddDate(0, 0, 1)
	u.tick(t.Context(), nextDay)
	expect("due, but the abandoned clone still drains")

	f.set("clone-id", func(c *fakeCtr) { c.running = false })
	u.tick(t.Context(), nextDay.Add(time.Minute))
	expect("cleanup", "remove l-abandoned")
}

// Nothing is created when the pulled image is the one already running.
func TestLinkUpdaterUpToDate(t *testing.T) {
	u, _, expect := newLinkUpdaterTest(t, map[string]*fakeCtr{
		"old-id": {name: "l", image: "sha256:new", running: true, labels: testLinkLabels},
	})
	u.tick(t.Context(), updateAt)
	expect("up to date", "pull docker.io/reg/gateway-link:latest")
}

func TestSwapKey(t *testing.T) {
	for _, c := range []struct{ warp, link, tried, want string }{
		{"new", "old", "", "new"},
		{"same", "same", "", ""},
		{"", "old", "", ""},       // warp not readable
		{"new", "", "", ""},       // a Link without LINK_BOOT_KEY is not a kit's
		{"new", "old", "new", ""}, // already tried, and the clone never came up
		{"newer", "old", "new", "newer"},
	} {
		if got := swapKey(c.warp, c.link, c.tried); got != c.want {
			t.Errorf("swapKey(%q, %q, %q) = %q, want %q", c.warp, c.link, c.tried, got, c.want)
		}
	}
}

func TestWithEnv(t *testing.T) {
	got := withEnv([]string{"A=1", "LINK_BOOT_KEY=old", "B=2", "LINK_BOOT_KEY=dup"}, "LINK_BOOT_KEY", "new")
	if fmt.Sprint(got) != "[A=1 B=2 LINK_BOOT_KEY=new]" {
		t.Errorf("withEnv = %q", got)
	}
}

// After `up -d warp node` with a rolled key, warp holds the new key and the Link
// still the old one. The node moves the Link onto it start-first, outside the
// daily update window and without pulling, and leaves it alone afterwards.
func TestLinkUpdaterSwapsTheLinkOntoWarpsNewKey(t *testing.T) {
	proj := map[string]string{"dylaris.component": "link", "com.docker.compose.project": "byon"}
	u, f, expect := newLinkUpdaterTest(t, map[string]*fakeCtr{
		"old-id":  {name: "l", image: "sha256:new", running: true, labels: proj, env: []string{"PATH=/new", "CORE_URL=x", "LINK_BOOT_KEY=old"}, log: edgeA},
		"warp-id": {name: "w", running: true, labels: map[string]string{"com.docker.compose.service": "warp", "com.docker.compose.project": "byon"}, env: []string{"API_KEY=new"}},
	})
	u.project = "byon"
	ctx := t.Context()
	early := updateAt.Add(-time.Hour) // not due: the swap does not wait for 04:00

	u.tick(ctx, early)
	expect("swap", "create l-next CORE_URL=x,LINK_BOOT_KEY=new", "start l-next")

	f.set("clone-id", func(c *fakeCtr) { c.log = edgeA })
	u.tick(ctx, early)
	expect("drain", "rename l l-draining", "update l-draining restart=no", "kill l-draining SIGTERM")

	f.set("old-id", func(c *fakeCtr) { c.running = false })
	u.tick(ctx, early)
	expect("finish, same image kept", "remove l-draining", "rename l-next l")

	u.tick(ctx, early)
	expect("on the new key: nothing to do")
}

// Without the compose project the warp label could name another stack's warp,
// so nothing is swapped.
func TestLinkUpdaterNeedsTheProjectToSwap(t *testing.T) {
	u, _, expect := newLinkUpdaterTest(t, map[string]*fakeCtr{
		"old-id":  {name: "l", image: "sha256:new", running: true, labels: testLinkLabels, env: []string{"LINK_BOOT_KEY=old"}},
		"warp-id": {name: "w", running: true, labels: map[string]string{"com.docker.compose.service": "warp"}, env: []string{"API_KEY=new"}},
	})
	u.tick(t.Context(), updateAt.Add(-time.Hour))
	expect("no project")
}

// A key the clone could not boot with (a typo in API_KEY, say) is abandoned
// like a slow image update, and not tried again every minute: only a different
// key starts another swap.
func TestLinkUpdaterTriesAFailedKeyOnce(t *testing.T) {
	proj := map[string]string{"dylaris.component": "link", "com.docker.compose.project": "byon"}
	warpLabels := map[string]string{"com.docker.compose.service": "warp", "com.docker.compose.project": "byon"}
	u, f, expect := newLinkUpdaterTest(t, map[string]*fakeCtr{
		"old-id":  {name: "l", image: "sha256:new", running: true, labels: proj, env: []string{"LINK_BOOT_KEY=old"}, log: edgeA},
		"warp-id": {name: "w", running: true, labels: warpLabels, env: []string{"API_KEY=typo"}},
	})
	u.project = "byon"
	early := updateAt.Add(-time.Hour)

	u.tick(t.Context(), early)
	expect("swap", "create l-next LINK_BOOT_KEY=typo", "start l-next")

	// The clone never came up and was cleaned away; the old Link still serves.
	f.set("clone-id", func(c *fakeCtr) { c.running = false })
	u.tick(t.Context(), early)
	expect("abort", "remove l-next")
	u.tick(t.Context(), early)
	expect("the same key is not tried again")

	f.set("warp-id", func(c *fakeCtr) { c.env = []string{"API_KEY=fixed"} })
	u.tick(t.Context(), early)
	expect("a corrected key is", "create l-next LINK_BOOT_KEY=fixed", "start l-next")
}

// A swap that falls in the daily window does not use it up: the image update
// runs once the swap is done. And the node says it can swap only while it
// knows its project and manages a Link.
func TestLinkUpdaterSwapKeepsTheDailyUpdateAndReportsReadiness(t *testing.T) {
	proj := map[string]string{"dylaris.component": "link", "com.docker.compose.project": "byon"}
	u, f, expect := newLinkUpdaterTest(t, map[string]*fakeCtr{
		"old-id":  {name: "l", image: "sha256:old", running: true, labels: proj, env: []string{"PATH=/old", "LINK_BOOT_KEY=old"}, log: edgeA},
		"warp-id": {name: "w", running: true, labels: map[string]string{"com.docker.compose.service": "warp", "com.docker.compose.project": "byon"}, env: []string{"API_KEY=new"}},
	})
	t.Cleanup(func() { linkKeySwapReady.Store(false) })

	u.tick(t.Context(), updateAt)
	if linkKeySwapReady.Load() {
		t.Error("ready without a compose project")
	}
	expect("no project: the daily update runs instead", "pull docker.io/reg/gateway-link:latest", "create l-next LINK_BOOT_KEY=old", "start l-next")
	f.set("clone-id", func(c *fakeCtr) { c.running = false })
	u.tick(t.Context(), updateAt)
	expect("cleanup", "remove l-next")

	u.project, u.lastRun = "byon", ""
	u.tick(t.Context(), updateAt)
	if !linkKeySwapReady.Load() {
		t.Error("not ready with a project and a Link")
	}
	expect("swap first", "create l-next LINK_BOOT_KEY=new", "start l-next")
	if u.lastRun != "" {
		t.Errorf("the swap used up the daily update (lastRun %q)", u.lastRun)
	}
}
