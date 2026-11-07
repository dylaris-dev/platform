package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"
)

const sharedDaemonUUID = "abababab-2222-3333-4444-555555555555"

// sharedDaemon is a Docker API holding one mc_<uuid> labelled with owner
// ("" = no label, "-" = no container at all), and recording every request
// that changes a container.
type sharedDaemon struct {
	mu      sync.Mutex
	changes []string
	env     []string // Config.Env of mc_<uuid>
	// noImage: a create answers 404 "No such image" until an image is pulled,
	// as the Engine API does (only the docker CLI pulls on create).
	noImage, pulled bool
}

func (d *sharedDaemon) changed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.changes...)
}

func newSharedDaemon(t *testing.T, owner string, running bool, onPull func()) (*DockerManager, *sharedDaemon) {
	t.Helper()
	// This node is node-a: no enrolled id in an empty secret dir, NODE_ID set.
	prev := nodeSecretDir
	nodeSecretDir = t.TempDir()
	t.Cleanup(func() { nodeSecretDir = prev })
	t.Setenv("NODE_ID", "node-a")

	d := &sharedDaemon{}
	labels := `{}`
	if owner != "" {
		labels = `{"` + ownerLabel + `":"` + owner + `"}`
	}
	run := "false"
	if running {
		run = "true"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/containers/mc_"+sharedDaemonUUID+"/json"):
			if owner == "-" {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"No such container"}`))
				return
			}
			d.mu.Lock()
			env, _ := json.Marshal(d.env)
			d.mu.Unlock()
			w.Write([]byte(`{"Id":"c-id","State":{"Running":` + run + `},"Config":{"Image":"img:1","WorkingDir":"/data/main","Env":` + string(env) + `,"Labels":` + labels + `},"HostConfig":{},"NetworkSettings":{"Networks":{}}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/containers/new-id-0123456789ab/json"):
			w.Write([]byte(`{"Id":"new-id-0123456789ab","State":{"Running":true},"Config":{"Labels":{}},"NetworkSettings":{"Networks":{"dylaris_net":{}}}}`))
		case strings.HasSuffix(p, "/images/create"):
			d.mu.Lock()
			d.pulled = true
			d.mu.Unlock()
			if onPull != nil {
				onPull()
			}
			w.Write([]byte(`{}`))
		case strings.HasSuffix(p, "/images/json"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/containers/json"):
			if owner == "-" || !running {
				w.Write([]byte(`[]`))
				return
			}
			w.Write([]byte(`[{"Id":"c-id","Names":["/mc_` + sharedDaemonUUID + `"],"Labels":` + labels + `}]`))
		case strings.HasSuffix(p, "/containers/create"):
			d.mu.Lock()
			missing := d.noImage && !d.pulled
			d.mu.Unlock()
			if missing {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"No such image: img:1"}`))
				return
			}
			d.record("create")
			w.Write([]byte(`{"Id":"new-id-0123456789ab"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/networks"):
			w.Write([]byte(`[{"Id":"0123456789abcdef0123","Name":"dylaris_net","IPAM":{"Config":[{"Subnet":"172.30.0.0/16","Gateway":"172.30.0.1"}]}}]`))
		case strings.Contains(p, "/networks/"):
			w.Write([]byte(`{"Id":"0123456789abcdef0123","Name":"dylaris_net","IPAM":{"Config":[{"Subnet":"172.30.0.0/16","Gateway":"172.30.0.1"}]}}`))
		case strings.Contains(p, "/containers/") && (r.Method == http.MethodDelete || r.Method == http.MethodPost):
			// start, stop, kill, remove: "<verb> <container>"
			parts := strings.Split(strings.TrimPrefix(p[strings.Index(p, "/containers/"):], "/containers/"), "/")
			verb := "remove"
			if len(parts) > 1 {
				verb = parts[1]
			}
			if verb == "wait" {
				w.Write([]byte(`{"StatusCode":0}`))
				return
			}
			d.record(verb + " " + parts[0])
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return &DockerManager{cli: cli, ctx: t.Context(), localDataPath: t.TempDir()}, d
}

func (d *sharedDaemon) record(s string) {
	d.mu.Lock()
	d.changes = append(d.changes, s)
	d.mu.Unlock()
}

// Two nodes on one docker.sock address the same mc_<uuid>. After a move A->B
// the source's migrate_cleanup ran PowerAction("delete") and force-removed the
// target's live server. Every by-name power action must leave a container
// labelled for another node alone, and keep acting on its own and on an
// unlabelled (pre-label) one.
func TestPowerActionLeavesAnotherNodesContainerAlone(t *testing.T) {
	for _, action := range []string{"start", "stop", "kill", "delete"} {
		for _, c := range []struct {
			owner string
			acts  bool
		}{
			{"node-b", false},
			{"node-a", true},
			{"", true}, // created before the label existed
		} {
			dm, d := newSharedDaemon(t, c.owner, true, nil)
			dm.PowerAction(sharedDaemonUUID, action)
			if got := len(d.changed()) > 0; got != c.acts {
				t.Errorf("%s on a container owned by %q: acted = %v (%v), want %v", action, c.owner, got, d.changed(), c.acts)
			}
		}
	}
}

// The port ledger is this node's own, so a delete still frees it when the
// container under that name is somebody else's.
func TestDeleteOfAForeignContainerStillReleasesThePort(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	dm, d := newSharedDaemon(t, "node-b", true, nil)
	dm.portMgr = NewPortManager(rdb, "node-a", 25600, 25699, seqMode)
	if _, err := dm.portMgr.AllocatePort(sharedDaemonUUID); err != nil {
		t.Fatal(err)
	}
	if err := dm.PowerAction(sharedDaemonUUID, "delete"); err != nil {
		t.Fatalf("delete of a foreign container: %v", err)
	}
	if len(d.changed()) != 0 {
		t.Fatalf("the other node's container was touched: %v", d.changed())
	}
	if got := dm.portMgr.GetPort(sharedDaemonUUID); got != 0 {
		t.Errorf("port %d still held after the delete", got)
	}
}

// The remove-before-create paths may replace another node's STOPPED leftover
// (a moved server taking over its name on a shared daemon) but never stop or
// remove a RUNNING one.
func TestCreateNeverReplacesAnotherNodesRunningContainer(t *testing.T) {
	for _, c := range []struct {
		owner    string
		running  bool
		replaces bool
	}{
		{"node-b", true, false},
		{"node-b", false, true},
		{"node-a", true, true},
		{"", true, true},
	} {
		for name, run := range map[string]func(*DockerManager, ServerConfig) error{
			"recreate":       (*DockerManager).RecreateWithCommand,
			"keep-run-state": (*DockerManager).RecreateKeepingRunState,
			"create-stopped": (*DockerManager).CreateServerPodStopped,
		} {
			dm, d := newSharedDaemon(t, c.owner, c.running, nil)
			cfg := ServerConfig{UUID: sharedDaemonUUID}
			cfg.Docker.Image = "img:1"
			run(dm, cfg)
			touched := false
			for _, ch := range d.changed() {
				if strings.HasSuffix(ch, " mc_"+sharedDaemonUUID) {
					touched = true
				}
			}
			if touched != c.replaces {
				t.Errorf("%s over a container of %q (running %v): touched = %v (%v), want %v", name, c.owner, c.running, touched, d.changed(), c.replaces)
			}
		}
	}
}

// A delete that lands during the image pull used to be followed by a create
// and start for a server Core no longer has, holding its port for good.
func TestADeleteDuringThePullLeavesNoContainer(t *testing.T) {
	const uuid = sharedDaemonUUID
	t.Cleanup(func() {
		deletedServers.Lock()
		delete(deletedServers.at, uuid)
		deletedServers.Unlock()
	})
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	dm, d := newSharedDaemon(t, "-", false, func() { markServerDeleted(uuid) })
	dm.portMgr = NewPortManager(rdb, "node-a", 25600, 25699, seqMode)
	cfg := ServerConfig{UUID: uuid}
	cfg.Docker.Image = "img:1"
	if err := dm.RecreateWithCommand(cfg); err == nil {
		t.Fatal("no error for a server deleted during its pull")
	}
	for _, ch := range d.changed() {
		if strings.HasPrefix(ch, "start") {
			t.Fatalf("a deleted server was started: %v", d.changed())
		}
	}
	if ch := d.changed(); strings.Contains(strings.Join(ch, ","), "create") && ch[len(ch)-1] != "remove new-id-0123456789ab" {
		t.Fatalf("the container created for a deleted server was left behind: %v", ch)
	}
	if got := dm.portMgr.GetPort(uuid); got != 0 {
		t.Errorf("port %d still held for a server deleted during its pull", got)
	}
}

// The target of a failed move kept its copy whenever any mc_<uuid> existed,
// and on a shared daemon that is always the source's.
func TestMigrateDiscardIgnoresAnotherNodesContainer(t *testing.T) {
	for _, c := range []struct {
		owner string
		kept  bool
	}{
		{"node-b", false},
		{"node-a", true},
		{"", true},
	} {
		dm, _ := newSharedDaemon(t, c.owner, false, nil)
		root := t.TempDir()
		world := filepath.Join(root, sharedDaemonUUID, "world", "level.dat")
		if err := os.MkdirAll(filepath.Dir(world), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(world, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		markMovedIn(root, sharedDaemonUUID, "attempt-1")
		handleMigrateDiscard(&StorageManager{paths: []string{root}}, dm, sharedDaemonUUID, "attempt-1", time.Second)
		kept := true
		for i := 0; i < 50; i++ {
			if _, err := os.Stat(world); os.IsNotExist(err) {
				kept = false
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if kept != c.kept {
			t.Errorf("container owned by %q: copy kept = %v, want %v", c.owner, kept, c.kept)
		}
	}
}

// gracefulStop is the stop the panel and the memory guard use: its console
// "stop" and its docker-stop fallback must not reach another node's server.
func TestGracefulStopLeavesAnotherNodesServerRunning(t *testing.T) {
	for _, c := range []struct {
		owner string
		stops bool
	}{
		{"node-b", false},
		{"node-a", true},
	} {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		dm, _ := newSharedDaemon(t, c.owner, true, nil)
		done := make(chan struct{})
		go func() { gracefulStop(rdb, sharedDaemonUUID, dm); close(done) }()
		key := "dylaris:server:" + sharedDaemonUUID + ":input"
		sent := false
		for i := 0; i < 250 && !sent; i++ {
			select {
			case <-done:
				i = 250
			default:
				time.Sleep(20 * time.Millisecond)
			}
			sent = mr.Exists(key)
		}
		if sent != c.stops {
			t.Errorf("container owned by %q: console stop sent = %v, want %v", c.owner, sent, c.stops)
		}
	}
}

// delete_sub_server's teardown and migrate_storage's remove call Docker by name
// directly rather than through PowerAction, so they carry the guard themselves.
func TestDirectByNameRemovesAskWhoseContainerItIs(t *testing.T) {
	for _, c := range []struct{ file, from, to, check string }{
		{"main.go", "teardown := func() {", "dm.cli.ContainerKill(killCtx, ref", "dm.ownedRef(mcName)"},
		{"docker_mgr.go", "func (dm *DockerManager) removeStaleServerContainer(", "dm.cli.ContainerRemove(ctx, info.ID", "ownsContainer(info.Config.Labels"},
	} {
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		i := strings.Index(s, c.from)
		j := strings.Index(s[max(i, 0):], c.to)
		if i < 0 || j < 0 {
			t.Fatalf("cannot locate %q; move this assertion with it", c.from)
		}
		if !strings.Contains(s[i:i+j], c.check) {
			t.Errorf("the by-name call after %q no longer checks the owner label", c.from)
		}
	}
}

// The owner check and the action were two by-name calls, so a container
// another node created under the name in between (a move on a shared daemon)
// was the one stopped, killed or removed. The action goes to the inspected ID.
func TestPowerActionActsOnTheContainerItChecked(t *testing.T) {
	for _, c := range []struct{ action, want string }{
		{"stop", "stop c-id"},
		{"kill", "kill c-id"},
		{"delete", "remove c-id"},
		{"start", "start c-id"},
	} {
		dm, d := newSharedDaemon(t, "node-a", c.action != "start", nil)
		dm.PowerAction(sharedDaemonUUID, c.action)
		if ch := d.changed(); len(ch) != 1 || ch[0] != c.want {
			t.Errorf("%s: %v, want [%s]", c.action, ch, c.want)
		}
	}
}

// P03: the reconciler's crash-restart (PowerAction "start") used to start the
// old container as it was, so after a host reboot - every MC container exited,
// ReconcileRedisEnv finds nothing running - a server came back on the Redis
// address and credential it was created with. A stale one is recreated now;
// a current one is still just started.
func TestCrashRestartRecreatesAStaleRedisEnv(t *testing.T) {
	prev := nodeRedis
	nodeRedis = &redisAddrState{}
	nodeRedis.set("10.0.0.9:6379", false)
	t.Cleanup(func() { nodeRedis = prev })

	for _, tc := range []struct {
		name     string
		env      []string
		recreate bool
	}{
		{"old address", []string{"REDIS_ADDR=10.0.0.1:6379"}, true},
		{"current", buildRedisEnv(sharedDaemonUUID, "", "10.0.0.9:6379"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dm, d := newSharedDaemon(t, "node-a", false, nil)
			d.env = tc.env
			dm.PowerAction(sharedDaemonUUID, "start")
			if got := slices.Contains(d.changed(), "create"); got != tc.recreate {
				t.Fatalf("recreated = %v (%v), want %v", got, d.changed(), tc.recreate)
			}
		})
	}
}

// P04: the stopped slot of a server's first create on a node whose daemon has
// never held the image failed with "No such image" (the Engine API does not
// pull on create), so main skipped saving the config and its disk limit.
func TestCreateStoppedPullsAMissingImage(t *testing.T) {
	dm, d := newSharedDaemon(t, "-", false, nil)
	d.noImage = true
	cfg := ServerConfig{UUID: sharedDaemonUUID}
	cfg.Docker.Image = "img:1"
	if err := dm.CreateServerPodStopped(cfg); err != nil {
		t.Fatalf("create of a slot whose image is not on the node: %v", err)
	}
	if !slices.Contains(d.changed(), "create") {
		t.Fatalf("no container was created: %v", d.changed())
	}
}

// P21: when the inspect finds no mc_<uuid>, a delete removed by NAME anyway, so
// on a shared daemon it force-removed the container another node created under
// the name a moment later. Not found is nothing to remove; the port is still
// released.
func TestDeleteOfAMissingContainerRemovesNothingByName(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	dm, d := newSharedDaemon(t, "-", false, nil)
	dm.portMgr = NewPortManager(rdb, "node-a", 25600, 25699, seqMode)
	if _, err := dm.portMgr.AllocatePort(sharedDaemonUUID); err != nil {
		t.Fatal(err)
	}
	if err := dm.PowerAction(sharedDaemonUUID, "delete"); err != nil {
		t.Fatalf("delete of a missing container: %v", err)
	}
	if ch := d.changed(); len(ch) != 0 {
		t.Fatalf("removed by name after a not-found inspect: %v", ch)
	}
	if got := dm.portMgr.GetPort(sharedDaemonUUID); got != 0 {
		t.Errorf("port %d still held after the delete", got)
	}
}

// Q04: the boot-time ReconcileRedisEnv runs beside the command listener now, so
// the heartbeat does not wait out a graceful stop per stale server. A recreate
// beside a command the user sent must not happen: it waits for the server's
// power lock like the reconciler does, and then still recreates.
func TestReconcileRedisEnvWaitsForTheServersPowerLock(t *testing.T) {
	prev := nodeRedis
	nodeRedis = &redisAddrState{}
	nodeRedis.set("10.0.0.9:6379", false)
	t.Cleanup(func() { nodeRedis = prev })

	dm, d := newSharedDaemon(t, "node-a", true, nil)
	d.env = []string{"REDIS_ADDR=10.0.0.1:6379"}

	release, ok := tryLockServerPower(sharedDaemonUUID)
	if !ok {
		t.Fatal("power lock already held")
	}
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	done := make(chan struct{})
	go func() { dm.ReconcileRedisEnv(rdb); close(done) }()

	time.Sleep(300 * time.Millisecond)
	if slices.Contains(d.changed(), "create") {
		release()
		t.Fatalf("recreated while a command held the power lock: %v", d.changed())
	}
	release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ReconcileRedisEnv did not finish after the lock came free")
	}
	if !slices.Contains(d.changed(), "create") {
		t.Fatalf("stale container not recreated once the lock was free: %v", d.changed())
	}
}

// R02/R03: the boot-time ReconcileRedisEnv lists the running servers once and
// may reach one minutes later. A server stopped in between (a user stop, the
// disk guard, a suspension) was recreated AND started, because an exited
// container keeps its stale env; and a kill during the recreate's own graceful
// stop was overridden. Only a server still running and meant to be online is
// recreated, and a stop noted during the recreate leaves it stopped.
func TestReconcileRedisEnvLeavesAStoppedServerStopped(t *testing.T) {
	prev := nodeRedis
	nodeRedis = &redisAddrState{}
	nodeRedis.set("10.0.0.9:6379", false)
	t.Cleanup(func() { nodeRedis = prev })

	for _, tc := range []struct {
		name          string
		running       bool
		desired       string
		killDuringRun bool
		recreate      bool
		start         bool
	}{
		{"running, meant online", true, "online", false, true, true},
		{"running, no desired state (old core)", true, "", false, true, true},
		{"stopped while it waited", false, "online", false, false, false},
		{"running but desired stopped", true, "stopped", false, false, false},
		{"kill during the recreate", true, "online", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lastStopIssued.Delete(sharedDaemonUUID)
			t.Cleanup(func() { lastStopIssued.Delete(sharedDaemonUUID) })
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			if tc.desired != "" {
				mr.Set(desiredStateKey(sharedDaemonUUID), tc.desired)
			}
			var onPull func()
			if tc.killDuringRun {
				onPull = func() { noteStop(sharedDaemonUUID, time.Now().Unix()) }
			}
			dm, d := newSharedDaemon(t, "node-a", tc.running, onPull)
			d.env = []string{"REDIS_ADDR=10.0.0.1:6379"}
			dm.reconcileRedisEnvOne(rdb, MCContainer{UUID: sharedDaemonUUID, ContainerName: "mc_" + sharedDaemonUUID}, "10.0.0.9:6379")
			ch := d.changed()
			if got := slices.Contains(ch, "create"); got != tc.recreate {
				t.Errorf("recreated = %v (%v), want %v", got, ch, tc.recreate)
			}
			if got := slices.Contains(ch, "start new-id-0123456789ab"); got != tc.start {
				t.Errorf("started = %v (%v), want %v", got, ch, tc.start)
			}
		})
	}
}
