package services

import (
	"context"
	"encoding/json"
	"testing"

	"dylaris-core/services/redisacl"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newQueueTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

// readStreamPayload reads the single entry expected on stream and unmarshals its
// "data" field (the JSON command body) into a generic map for field assertions.
func readStreamPayload(t *testing.T, rdb *redis.Client, stream string) map[string]interface{} {
	t.Helper()
	msgs, err := rdb.XRange(context.Background(), stream, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange %s: %v", stream, err)
	}
	if len(msgs) != 1 {
		t.Fatalf("stream %s: got %d entries, want 1", stream, len(msgs))
	}
	raw, ok := msgs[0].Values["data"].(string)
	if !ok {
		t.Fatalf("stream %s: entry has no string data field: %+v", stream, msgs[0].Values)
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("stream %s: unmarshal payload: %v", stream, err)
	}
	return out
}

func TestAclRelevantAction(t *testing.T) {
	cases := []struct {
		action string
		want   bool
	}{
		{"create", true},
		{"setup", true},
		{"reinstall", true},
		{"switch_server", true},
		{"update_resources", true},
		{"start", false},
		{"stop", false},
		{"restart", false},
		{"kill", false},
		{"delete", false},
		{"delete_sub_server", false},
		{"install_mod", false},
		{"remove_mod", false},
		{"migrate_storage", false},
		{"migrate_in", false},
		{"backup_run", false},
		{"backup_restore", false},
		{"proxy_network_create", false},
		{"", false},
		{"CREATE", false}, // case-sensitive: must not match uppercase
	}
	for _, c := range cases {
		if got := aclRelevantAction(c.action); got != c.want {
			t.Errorf("aclRelevantAction(%q) = %v, want %v", c.action, got, c.want)
		}
	}
}

func TestSendCommand_PublishesToNodeCmdStream(t *testing.T) {
	rdb := newQueueTestRedis(t)
	svc := NewQueueService(rdb)

	config := map[string]interface{}{"uuid": "srv-1", "ram": float64(1024)}
	installer := map[string]interface{}{"kind": "vanilla"}

	if err := svc.SendCommand(context.Background(), "tok-1", "create", config, installer); err != nil {
		t.Fatalf("SendCommand: %v", err)
	}

	payload := readStreamPayload(t, rdb, "dylaris:node:tok-1:cmds")
	if payload["action"] != "create" {
		t.Errorf("action = %v, want create", payload["action"])
	}
	gotConfig, ok := payload["config"].(map[string]interface{})
	if !ok || gotConfig["uuid"] != "srv-1" {
		t.Errorf("config = %+v, want uuid=srv-1", payload["config"])
	}
	gotInstaller, ok := payload["installer"].(map[string]interface{})
	if !ok || gotInstaller["kind"] != "vanilla" {
		t.Errorf("installer = %+v, want kind=vanilla", payload["installer"])
	}
}

func TestSendCommand_OmitsInstallerWhenNil(t *testing.T) {
	rdb := newQueueTestRedis(t)
	svc := NewQueueService(rdb)

	if err := svc.SendCommand(context.Background(), "tok-2", "stop", map[string]interface{}{"uuid": "srv-2"}, nil); err != nil {
		t.Fatalf("SendCommand: %v", err)
	}

	payload := readStreamPayload(t, rdb, "dylaris:node:tok-2:cmds")
	if _, present := payload["installer"]; present {
		t.Errorf("installer field must be omitted when nil, got %+v", payload)
	}
}

// TestSendCommand_ACLEnsureFailureDoesNotBlockPublish exercises the pre-placement
// ACL ensure path (aclRelevantAction gate + Handshake.EnsureForToken) wired to a
// real Handshake/Provisioner pointed at miniredis. miniredis does not implement
// the ACL command (see redisacl package note), so EnsureForToken's underlying
// provisioning always errors here — this test pins that SendCommand's doc-promised
// behavior holds: an ACL ensure failure is logged and swallowed, never blocking
// the command from reaching the node's stream.
func TestSendCommand_ACLEnsureFailureDoesNotBlockPublish(t *testing.T) {
	rdb := newQueueTestRedis(t)
	svc := NewQueueService(rdb)

	prov := redisacl.NewProvisioner(rdb)
	store := &fakeACLHandshakeStore{secrets: map[int]string{}, knownToken: "tok-3", knownNodeID: 1}
	h := redisacl.NewHandshake(store, prov, "test-cluster-secret")
	svc.SetACL(h)

	// "create" is ACL-relevant, so this must attempt EnsureForToken, which will
	// fail (ACL SETUSER unsupported by miniredis) and be swallowed.
	if err := svc.SendCommand(context.Background(), "tok-3", "create", map[string]interface{}{"uuid": "srv-3"}, nil); err != nil {
		t.Fatalf("SendCommand must succeed even though ACL ensure fails, got: %v", err)
	}

	payload := readStreamPayload(t, rdb, "dylaris:node:tok-3:cmds")
	if payload["action"] != "create" {
		t.Errorf("action = %v, want create", payload["action"])
	}
}

// fakeACLHandshakeStore is a minimal redisacl.HandshakeStore for the ACL-ensure
// swallow test above. Only NodeIDByToken and the secret accessors are exercised;
// the rest satisfy the interface but are never called on this path.
type fakeACLHandshakeStore struct {
	secrets     map[int]string
	knownToken  string
	knownNodeID int
}

func (f *fakeACLHandshakeStore) GetNodeSecretEnc(id int) (string, error) { return f.secrets[id], nil }
func (f *fakeACLHandshakeStore) SetNodeSecretEnc(id int, enc string) error {
	f.secrets[id] = enc
	return nil
}
func (f *fakeACLHandshakeStore) SetNodeSecretEncIfUnchanged(id int, prev, next string) (bool, error) {
	if f.secrets[id] != prev {
		return false, nil
	}
	f.secrets[id] = next
	return true, nil
}
func (f *fakeACLHandshakeStore) ServerUUIDsByNode(nodeID int) ([]string, error) { return nil, nil }
func (f *fakeACLHandshakeStore) ResolveEnrollToken(plaintext string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeACLHandshakeStore) ConsumeEnrollToken(plaintext string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeACLHandshakeStore) NodeLimitReached(ownerID string) bool { return false }
func (f *fakeACLHandshakeStore) CreateBYONNode(token, address, ownerID, displayName string) (int, error) {
	return 0, nil
}
func (f *fakeACLHandshakeStore) CreatePlatformNode(token, address, displayName string) (int, error) {
	return 0, nil
}
func (f *fakeACLHandshakeStore) NodeIDByToken(token string) (int, bool, error) {
	if token == f.knownToken {
		return f.knownNodeID, true, nil
	}
	return 0, false, nil
}

func TestSendRawCommand_PublishesPayloadAsIs(t *testing.T) {
	rdb := newQueueTestRedis(t)
	svc := NewQueueService(rdb)

	payload := map[string]interface{}{
		"action": "backup_run",
		"uuid":   "srv-4",
		"target": "s3://bucket/key",
	}
	if err := svc.SendRawCommand(context.Background(), "tok-4", payload); err != nil {
		t.Fatalf("SendRawCommand: %v", err)
	}

	got := readStreamPayload(t, rdb, "dylaris:node:tok-4:cmds")
	if got["action"] != "backup_run" || got["uuid"] != "srv-4" || got["target"] != "s3://bucket/key" {
		t.Errorf("payload = %+v, want action/uuid/target preserved as-is", got)
	}
}

func TestSendMigrateCommand(t *testing.T) {
	rdb := newQueueTestRedis(t)
	svc := NewQueueService(rdb)

	if err := svc.SendMigrateCommand(context.Background(), "tok-7", "server-uuid", "/data/new-path"); err != nil {
		t.Fatalf("SendMigrateCommand: %v", err)
	}

	got := readStreamPayload(t, rdb, "dylaris:node:tok-7:cmds")
	if got["action"] != "migrate_storage" {
		t.Errorf("action = %v, want migrate_storage", got["action"])
	}
	cfg, _ := got["config"].(map[string]interface{})
	if cfg["uuid"] != "server-uuid" {
		t.Errorf("config.uuid = %v, want server-uuid", cfg["uuid"])
	}
	if got["targetPath"] != "/data/new-path" {
		t.Errorf("targetPath = %v, want /data/new-path", got["targetPath"])
	}
}

func TestSendMigrateOutCommand(t *testing.T) {
	rdb := newQueueTestRedis(t)
	svc := NewQueueService(rdb)

	if err := svc.SendMigrateOutCommand(context.Background(), "tok-8", "server-uuid"); err != nil {
		t.Fatalf("SendMigrateOutCommand: %v", err)
	}

	got := readStreamPayload(t, rdb, "dylaris:node:tok-8:cmds")
	if got["action"] != "migrate_out" {
		t.Errorf("action = %v, want migrate_out", got["action"])
	}
	cfg, _ := got["config"].(map[string]interface{})
	if cfg["uuid"] != "server-uuid" {
		t.Errorf("config.uuid = %v, want server-uuid", cfg["uuid"])
	}
}

func TestSendMigrateInCommand(t *testing.T) {
	rdb := newQueueTestRedis(t)
	svc := NewQueueService(rdb)

	if err := svc.SendMigrateInCommand(context.Background(), "tok-9", "server-uuid", "source-node-id", "migrate-tok", "deadbeef", 4096, []string{"10.0.0.5", "192.168.1.10"}); err != nil {
		t.Fatalf("SendMigrateInCommand: %v", err)
	}

	got := readStreamPayload(t, rdb, "dylaris:node:tok-9:cmds")
	if got["action"] != "migrate_in" {
		t.Errorf("action = %v, want migrate_in", got["action"])
	}
	cfg, _ := got["config"].(map[string]interface{})
	if cfg["uuid"] != "server-uuid" {
		t.Errorf("config.uuid = %v, want server-uuid", cfg["uuid"])
	}
	if got["sourceNodeId"] != "source-node-id" {
		t.Errorf("sourceNodeId = %v, want source-node-id", got["sourceNodeId"])
	}
	if got["migrateToken"] != "migrate-tok" {
		t.Errorf("migrateToken = %v, want migrate-tok", got["migrateToken"])
	}
	if got["expectedSha256"] != "deadbeef" {
		t.Errorf("expectedSha256 = %v, want deadbeef", got["expectedSha256"])
	}
	// The download bound. The sha256 travels with it but cannot replace it: the
	// target checks the hash only after the last byte is on its disk.
	if size, _ := got["expectedSize"].(float64); int64(size) != 4096 {
		t.Errorf("expectedSize = %v, want 4096; without it the target pulls unbounded", got["expectedSize"])
	}
	ips, ok := got["sourcePrivateIps"].([]interface{})
	if !ok || len(ips) != 2 || ips[0] != "10.0.0.5" || ips[1] != "192.168.1.10" {
		t.Errorf("sourcePrivateIps = %+v, want [10.0.0.5 192.168.1.10]", got["sourcePrivateIps"])
	}
}

func TestSendMigrateInCommand_OmitsEmptySourcePrivateIPs(t *testing.T) {
	rdb := newQueueTestRedis(t)
	svc := NewQueueService(rdb)

	if err := svc.SendMigrateInCommand(context.Background(), "tok-10", "server-uuid", "source-node-id", "migrate-tok", "deadbeef", 4096, nil); err != nil {
		t.Fatalf("SendMigrateInCommand: %v", err)
	}

	got := readStreamPayload(t, rdb, "dylaris:node:tok-10:cmds")
	if _, present := got["sourcePrivateIps"]; present {
		t.Errorf("sourcePrivateIps must be omitted when nil/empty, got %+v", got)
	}
}

func TestSendMigratePushR2Command(t *testing.T) {
	rdb := newQueueTestRedis(t)
	svc := NewQueueService(rdb)

	if err := svc.SendMigratePushR2Command(context.Background(), "tok-11", "server-uuid", "https://example.com/put-url"); err != nil {
		t.Fatalf("SendMigratePushR2Command: %v", err)
	}

	got := readStreamPayload(t, rdb, "dylaris:node:tok-11:cmds")
	if got["action"] != "migrate_push_r2" {
		t.Errorf("action = %v, want migrate_push_r2", got["action"])
	}
	cfg, _ := got["config"].(map[string]interface{})
	if cfg["uuid"] != "server-uuid" {
		t.Errorf("config.uuid = %v, want server-uuid", cfg["uuid"])
	}
	if got["presignedPutUrl"] != "https://example.com/put-url" {
		t.Errorf("presignedPutUrl = %v, want https://example.com/put-url", got["presignedPutUrl"])
	}
}

func TestSendMigratePullR2Command(t *testing.T) {
	rdb := newQueueTestRedis(t)
	svc := NewQueueService(rdb)

	if err := svc.SendMigratePullR2Command(context.Background(), "tok-12", "server-uuid", "https://example.com/get-url", "cafebabe", 8192); err != nil {
		t.Fatalf("SendMigratePullR2Command: %v", err)
	}

	got := readStreamPayload(t, rdb, "dylaris:node:tok-12:cmds")
	if got["action"] != "migrate_pull_r2" {
		t.Errorf("action = %v, want migrate_pull_r2", got["action"])
	}
	cfg, _ := got["config"].(map[string]interface{})
	if cfg["uuid"] != "server-uuid" {
		t.Errorf("config.uuid = %v, want server-uuid", cfg["uuid"])
	}
	if got["presignedGetUrl"] != "https://example.com/get-url" {
		t.Errorf("presignedGetUrl = %v, want https://example.com/get-url", got["presignedGetUrl"])
	}
	if got["expectedSha256"] != "cafebabe" {
		t.Errorf("expectedSha256 = %v, want cafebabe", got["expectedSha256"])
	}
	// The R2 leg pulls from a pre-signed URL and needs the same bound as the
	// node-to-node leg; a pre-signed URL says nothing about the response size.
	if size, _ := got["expectedSize"].(float64); int64(size) != 8192 {
		t.Errorf("expectedSize = %v, want 8192; without it the target pulls unbounded", got["expectedSize"])
	}
}

func TestSendMigrateCleanupCommand(t *testing.T) {
	rdb := newQueueTestRedis(t)
	svc := NewQueueService(rdb)

	if err := svc.SendMigrateCleanupCommand(context.Background(), "tok-13", "server-uuid"); err != nil {
		t.Fatalf("SendMigrateCleanupCommand: %v", err)
	}

	got := readStreamPayload(t, rdb, "dylaris:node:tok-13:cmds")
	if got["action"] != "migrate_cleanup" {
		t.Errorf("action = %v, want migrate_cleanup", got["action"])
	}
	cfg, _ := got["config"].(map[string]interface{})
	if cfg["uuid"] != "server-uuid" {
		t.Errorf("config.uuid = %v, want server-uuid", cfg["uuid"])
	}
}
