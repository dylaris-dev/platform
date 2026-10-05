package services

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/pkg/crypto"
	"dylaris-core/store"
	"dylaris-pkg/queue"
)

const (
	flowSourceToken = "11111111-aaaa-4aaa-8aaa-000000000001"
	flowTargetToken = "22222222-bbbb-4bbb-8bbb-000000000002"
	flowCluster     = "cluster-secret-for-tests"
)

// flowStore holds one server on node 1 and the two nodes.
type flowStore struct {
	store.Store
	mu       sync.Mutex
	srv      models.Server
	nodes    map[int]*models.Node
	enc      string
	cutovers []int
}

func (f *flowStore) GetServerByID(int) (*models.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.srv
	return &c, nil
}
func (f *flowStore) GetNodeByID(id int) (*models.Node, error) {
	n, ok := f.nodes[id]
	if !ok {
		return nil, errors.New("no such node")
	}
	c := *n
	return &c, nil
}
func (f *flowStore) GetSetting(key string) (string, error) {
	if key == "routing_mode" {
		return "gateway", nil
	}
	return "", nil
}
func (f *flowStore) UpdateServerStatus(_ int, status string) error {
	f.mu.Lock()
	f.srv.Status = status
	f.mu.Unlock()
	return nil
}
func (f *flowStore) UpdateServerDesiredState(int, string) error { return nil }
func (f *flowStore) UpdateServerNode(_ int, node int) error {
	f.mu.Lock()
	f.srv.NodeID = node
	f.cutovers = append(f.cutovers, node)
	f.mu.Unlock()
	return nil
}
func (f *flowStore) GetNodeSecretEnc(int) (string, error) { return f.enc, nil }

func (f *flowStore) status() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.srv.Status
}

type flowGateway struct{ GatewayProvider }

func (flowGateway) MigrateServerRoutes(uint, uint) error { return nil }

// simNodes plays both nodes: it reads their command streams and answers the
// way the real ones do, after a short delay so the orchestrator's first poll
// always comes first.
type simNodes struct {
	mu          sync.Mutex
	targetFails bool
	// breakTarget makes every later command to the target fail to queue.
	breakTarget bool
	got         map[string][]map[string]interface{} // token -> commands
}

func (s *simNodes) commands(token, action string) []map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]interface{}
	for _, c := range s.got[token] {
		if c["action"] == action {
			out = append(out, c)
		}
	}
	return out
}

func (s *simNodes) run(ctx context.Context, rdb *redis.Client) {
	last := map[string]string{"dylaris:node:" + flowSourceToken + ":cmds": "0", "dylaris:node:" + flowTargetToken + ":cmds": "0"}
	for ctx.Err() == nil {
		streams := []string{}
		ids := []string{}
		for k, v := range last {
			streams = append(streams, k)
			ids = append(ids, v)
		}
		res, err := rdb.XRead(ctx, &redis.XReadArgs{Streams: append(streams, ids...), Block: 50 * time.Millisecond}).Result()
		if err != nil {
			continue
		}
		for _, st := range res {
			token := flowSourceToken
			if st.Stream == "dylaris:node:"+flowTargetToken+":cmds" {
				token = flowTargetToken
			}
			for _, m := range st.Messages {
				last[st.Stream] = m.ID
				var cmd map[string]interface{}
				_ = json.Unmarshal([]byte(m.Values["data"].(string)), &cmd)
				s.mu.Lock()
				if s.got == nil {
					s.got = map[string][]map[string]interface{}{}
				}
				s.got[token] = append(s.got[token], cmd)
				fails, brk := s.targetFails, s.breakTarget
				s.mu.Unlock()
				uuid, _ := cmd["config"].(map[string]interface{})["uuid"].(string)
				attempt, _ := cmd["attempt"].(string)
				progress := queue.MigrationProgressID(uuid, attempt)
				go func(action string) {
					time.Sleep(300 * time.Millisecond)
					switch action {
					case "migrate_out":
						rdb.Set(ctx, queue.MigrationMetaKey(token, progress), `{"sha256":"abc","size":10}`, time.Hour)
						rdb.Set(ctx, queue.MigrationStatusKey(token, progress), `{"phase":"staged"}`, time.Hour)
					case "migrate_in":
						phase := `{"phase":"transferred"}`
						if fails {
							phase = `{"phase":"error","error":"extract failed"}`
						}
						if brk {
							// A string where the stream was: XADD answers WRONGTYPE.
							stream := "dylaris:node:" + flowTargetToken + ":cmds"
							rdb.Del(ctx, stream)
							rdb.Set(ctx, stream, "x", 0)
						}
						rdb.Set(ctx, queue.MigrationStatusKey(token, progress), phase, time.Hour)
					}
				}(cmd["action"].(string))
			}
		}
	}
}

func newFlow(t *testing.T, status string) (*MigrationOrchestrator, *flowStore, *simNodes, *redis.Client) {
	t.Helper()
	rdb := newQueueTestRedis(t)
	enc, err := crypto.Encrypt(crypto.DeriveKey(flowCluster, "node-redis-secret"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	fs := &flowStore{
		srv: models.Server{ID: 5, UUID: "srv-flow", NodeID: 1, Status: status},
		nodes: map[int]*models.Node{
			1: {ID: 1, Token: flowSourceToken, Status: "online"},
			2: {ID: 2, Token: flowTargetToken, Status: "online"},
		},
		enc: enc,
	}
	// Both nodes heartbeat; a development build names no release.
	for _, tok := range []string{flowSourceToken, flowTargetToken} {
		hb, _ := json.Marshal(NodeHeartbeat{ID: tok})
		rdb.Set(context.Background(), "dylaris:discovery:"+tok, hb, 0)
	}
	o := NewMigrationOrchestrator(fs, rdb, NewQueueService(rdb), flowGateway{}, flowCluster)
	sim := &simNodes{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sim.run(ctx, rdb)
	return o, fs, sim, rdb
}

// The pull endpoint is published under the source's TOKEN; the target was told
// the numeric node id, under which nothing exists, so every move failed.
func TestAMoveNamesTheSourceByTheIDItPublishesUnder(t *testing.T) {
	o, fs, sim, _ := newFlow(t, "stopped")
	o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual", RequestedBy: "admin"})
	in := sim.commands(flowTargetToken, "migrate_in")
	if len(in) != 1 || in[0]["sourceNodeId"] != flowSourceToken {
		t.Fatalf("migrate_in = %v, want sourceNodeId = the source's token", in)
	}
	if len(fs.cutovers) != 1 || fs.cutovers[0] != 2 {
		t.Fatalf("cutovers %v, want onto node 2", fs.cutovers)
	}
}

// An operator's suspension moves with the server: the move used to end in
// "stopped", which the power gate does not refuse.
func TestASuspendedServerStaysSuspendedAfterAMove(t *testing.T) {
	o, fs, _, _ := newFlow(t, "suspended")
	o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
	if got := fs.status(); got != "suspended" {
		t.Fatalf("status after the move = %q, want suspended", got)
	}
}

// Progress keys from an earlier attempt live for an hour. Read as this
// attempt's, a leftover "transferred" cut over onto the old copy and had the
// source's current data deleted - here, while the target actually failed.
func TestAnEarlierAttemptsProgressIsNotReadAsThisOnes(t *testing.T) {
	o, fs, sim, rdb := newFlow(t, "stopped")
	sim.targetFails = true
	ctx := context.Background()
	rdb.Set(ctx, queue.MigrationStatusKey(flowTargetToken, "srv-flow"), `{"phase":"transferred"}`, time.Hour)
	rdb.Set(ctx, queue.MigrationStatusKey(flowSourceToken, "srv-flow"), `{"phase":"staged"}`, time.Hour)
	rdb.Set(ctx, queue.MigrationMetaKey(flowSourceToken, "srv-flow"), `{"sha256":"old","size":10}`, time.Hour)
	o.Migrate(ctx, MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
	if len(fs.cutovers) != 0 {
		t.Fatalf("cut over on a stale progress key: %v", fs.cutovers)
	}
	if len(sim.commands(flowSourceToken, "migrate_cleanup")) != 0 {
		t.Fatal("the source's data was cleaned up after a failed move")
	}
}

// A failed move tells the source to drop the archive it staged; it used to
// stay, a full copy outside the server's quota, past even its deletion.
func TestAFailedMoveDropsTheStagedArchive(t *testing.T) {
	o, _, sim, _ := newFlow(t, "stopped")
	sim.targetFails = true
	o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
	if len(sim.commands(flowSourceToken, "migrate_abort")) != 1 {
		t.Fatal("the source was not told to drop its staged archive")
	}
}

// A request runs against the placement it was decided on, or not at all.
func TestARequestForAServerThatMovedSinceDoesNothing(t *testing.T) {
	o, fs, sim, _ := newFlow(t, "stopped")
	o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 9, Reason: "rebalance"})
	time.Sleep(400 * time.Millisecond)
	if len(fs.cutovers) != 0 || len(sim.commands(flowSourceToken, "migrate_out")) != 0 {
		t.Fatal("a stale request moved the server")
	}
}

// A source that is not there fails the move before anything is stopped.
func TestAnOfflineSourceFailsBeforeAnythingHappens(t *testing.T) {
	o, fs, sim, _ := newFlow(t, "online")
	fs.nodes[1].Status = "offline"
	o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
	time.Sleep(400 * time.Millisecond)
	if fs.status() != "online" || len(sim.commands(flowSourceToken, "stop")) != 0 || len(sim.commands(flowSourceToken, "migrate_out")) != 0 {
		t.Fatalf("an offline source's server was touched: status %q", fs.status())
	}
}

// One request per server: the queue runs one move at a time for the whole
// platform, and repeating a transfer held it for everyone.
func TestASecondRequestForTheSameServerIsRefused(t *testing.T) {
	o, _, _, rdb := newFlow(t, "stopped")
	ctx := context.Background()
	if err := o.EnqueueMigration(ctx, 5, 2, "transfer", "u1"); err != nil {
		t.Fatal(err)
	}
	if err := o.EnqueueMigration(ctx, 5, 2, "transfer", "u1"); !errors.Is(err, ErrMigrationQueued) {
		t.Fatalf("second request: %v, want ErrMigrationQueued", err)
	}
	payload := readStreamPayload(t, rdb, migrationStreamKey)
	if int(payload["sourceNodeID"].(float64)) != 1 {
		t.Fatalf("the request does not carry its source: %v", payload)
	}
	if payload["preStatus"] != "stopped" {
		t.Fatalf("the request does not carry the status to return to: %v", payload)
	}
	// Once it has run, the server can be moved again.
	o.Migrate(ctx, MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "transfer"})
	if err := o.EnqueueMigration(ctx, 5, 1, "transfer", "u1"); err != nil {
		t.Fatalf("after the move ran: %v", err)
	}
}

// An earlier attempt keeps running on the target after Core gave up on it, and
// its late "transferred" landed in the key the next attempt was waiting on:
// Core cut over onto the older copy and deleted the source's current data.
// Each attempt reports under its own id now, so a late write lands elsewhere.
func TestALateReportFromAnEarlierAttemptIsNotThisOnes(t *testing.T) {
	o, fs, sim, rdb := newFlow(t, "stopped")
	sim.targetFails = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The earlier attempt's migrate_in finishing late, under the BARE uuid as
	// well, while this attempt runs.
	go func() {
		for ctx.Err() == nil {
			rdb.Set(ctx, queue.MigrationStatusKey(flowTargetToken, "srv-flow"), `{"phase":"transferred"}`, time.Hour)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
	if len(fs.cutovers) != 0 {
		t.Fatalf("cut over on another attempt's report: %v", fs.cutovers)
	}
}

// A node too old to report per attempt is refused before anything happens: its
// reports would never be read, and the move would time out with the server
// stopped.
func TestAMoveWithAnOldNodeIsRefusedUpFront(t *testing.T) {
	o, fs, sim, rdb := newFlow(t, "online")
	hb, _ := json.Marshal(NodeHeartbeat{ID: flowTargetToken, ReleaseVersion: "2026.10.01"})
	rdb.Set(context.Background(), "dylaris:discovery:"+flowTargetToken, hb, 0)
	o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
	time.Sleep(400 * time.Millisecond)
	if fs.status() != "online" || len(sim.commands(flowSourceToken, "migrate_out")) != 0 {
		t.Fatalf("a move with an old node went ahead: status %q", fs.status())
	}
}

// A re-run after a Core crash reads "migrating" from the database. A return
// that set no status left it there for good - the status watcher leaves
// "migrating" alone - and a successful re-run lost a suspension. The request
// carries the status the server had when the move was decided.
func TestARerunPutsBackTheStatusTheServerHad(t *testing.T) {
	t.Run("a re-run that stops early", func(t *testing.T) {
		o, fs, _, _ := newFlow(t, "migrating")
		o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 9, PreStatus: "suspended", Reason: "manual"})
		if got := fs.status(); got != "suspended" {
			t.Fatalf("status %q, want suspended back", got)
		}
	})
	t.Run("a re-run that completes", func(t *testing.T) {
		o, fs, _, _ := newFlow(t, "migrating")
		o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, PreStatus: "suspended", Reason: "manual"})
		if got := fs.status(); got != "suspended" {
			t.Fatalf("status %q, want suspended after the move", got)
		}
	})
}

// The target starts a moved server from the config Core sends, never from the
// one in the archive: a customer's source node wrote that one. Sent in either
// run state, since a stopped server is started from it later.
func TestTheTargetGetsCoresConfigAfterAMove(t *testing.T) {
	o, fs, sim, _ := newFlow(t, "stopped")
	fs.srv.GameImage = "ghcr.io/dylaris-dev/java:21"
	fs.srv.Memory = 4096
	fs.srv.ActiveSubServer = "survival"
	fs.srv.ExtraJvmFlags = "-Dx=1"
	o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
	time.Sleep(100 * time.Millisecond)
	got := sim.commands(flowTargetToken, "reconfigure")
	if len(got) != 1 {
		t.Fatalf("reconfigure sent %d times, want once", len(got))
	}
	cfg := got[0]["config"].(map[string]interface{})
	d := cfg["docker"].(map[string]interface{})
	if cfg["activeSubServer"] != "survival" || d["image"] != "ghcr.io/dylaris-dev/java:21" || d["ram"] != float64(4096) || d["extraJvmFlags"] != DefaultJvmFlags+" -Dx=1" {
		t.Fatalf("reconfigure carried %v", cfg)
	}
}

// An older target still starts from the archived config, so it cannot take a
// move, whatever the source runs.
func TestATargetThatTrustsTheArchiveIsRefused(t *testing.T) {
	o, fs, sim, rdb := newFlow(t, "stopped")
	hb, _ := json.Marshal(NodeHeartbeat{ID: flowTargetToken, ReleaseVersion: "2026.10.05"})
	rdb.Set(context.Background(), "dylaris:discovery:"+flowTargetToken, hb, 0)
	o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
	time.Sleep(400 * time.Millisecond)
	if len(fs.cutovers) != 0 || len(sim.commands(flowSourceToken, "migrate_out")) != 0 {
		t.Fatalf("a move onto a target older than %s went ahead", migrationTargetSince)
	}
}

// A target that never got Core's config has nothing to start from. The move
// said "done" over that failure, and the server read "starting" for good.
func TestAMoveWhoseConfigNeverReachedTheTargetSaysSo(t *testing.T) {
	o, fs, sim, rdb := newFlow(t, "stopped")
	sim.mu.Lock()
	sim.breakTarget = true
	sim.mu.Unlock()
	o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
	if len(fs.cutovers) != 1 {
		t.Fatalf("cutovers %v, want the move to have cut over", fs.cutovers)
	}
	raw, _ := rdb.Get(context.Background(), "dylaris:migration:srv-flow:orchestration").Result()
	var st orchestrationStatus
	json.Unmarshal([]byte(raw), &st)
	if st.Phase != "failed_post_cutover" {
		t.Errorf("phase %q, want failed_post_cutover", st.Phase)
	}
	if got := fs.status(); got != "stopped" {
		t.Errorf("status %q, want stopped", got)
	}
}
