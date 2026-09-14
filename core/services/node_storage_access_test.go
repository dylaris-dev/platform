package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"
)

// storageFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only GetSetting is overridden.
type storageFakeStore struct {
	store.Store
	settings map[string]string
}

func (f *storageFakeStore) GetSetting(key string) (string, error) {
	v, ok := f.settings[key]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

// presignTTL no longer reaches backups or restores (their URLs are minted when
// the transfer starts), but the cross-LAN migration still presigns with it.
func TestPresignTTL(t *testing.T) {
	cases := []struct {
		name     string
		isBYON   bool
		settings map[string]string
		want     time.Duration
	}{
		{"node default, no setting", false, nil, 60 * time.Minute},
		{"byon default, no setting", true, nil, 360 * time.Minute},
		{"node custom valid", false, map[string]string{"r2.presign_ttl_node_minutes": "15"}, 15 * time.Minute},
		{"byon custom valid", true, map[string]string{"r2.presign_ttl_byon_minutes": "120"}, 120 * time.Minute},
		{"node non-numeric falls back", false, map[string]string{"r2.presign_ttl_node_minutes": "abc"}, 60 * time.Minute},
		{"node zero falls back", false, map[string]string{"r2.presign_ttl_node_minutes": "0"}, 60 * time.Minute},
		{"node negative falls back", false, map[string]string{"r2.presign_ttl_node_minutes": "-5"}, 60 * time.Minute},
		{"byon setting does not affect node key", false, map[string]string{"r2.presign_ttl_byon_minutes": "999"}, 60 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &storageFakeStore{settings: c.settings}
			got := presignTTL(st, c.isBYON)
			if got != c.want {
				t.Errorf("presignTTL(isBYON=%v) = %v, want %v", c.isBYON, got, c.want)
			}
		})
	}
}

func s3Storage(config string) *models.BackupStorage {
	return &models.BackupStorage{ID: 1, Provider: "s3", Config: json.RawMessage(config)}
}

func byonNode() *models.Node {
	owner := "owner-1"
	return &models.Node{ID: 1, Token: "node-current", OwnerID: &owner}
}

func operatorNode() *models.Node {
	return &models.Node{ID: 1, Token: "node-current", OwnerID: nil}
}

// heartbeatRedis is a Redis holding one node heartbeat per token, reporting the
// given release version (empty: a heartbeat that names none).
func heartbeatRedis(t *testing.T, versions map[string]string) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	for token, v := range versions {
		hb, _ := json.Marshal(NodeHeartbeat{ID: token, ReleaseVersion: v})
		mr.Set("dylaris:discovery:"+token, string(hb))
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

// currentNodes is a Redis in which "node-current" runs the first release that
// takes presigned-only transfers.
func currentNodes(t *testing.T) *redis.Client {
	return heartbeatRedis(t, map[string]string{"node-current": presignedMultipartSince})
}

// FLIPPED (document F), formerly _OperatorNode_ReturnsFullBlobNoURL and
// _BYONS3_StripsCredentials. An operator node on a plain s3 row used to receive
// the full blob, secret access key included, in its command stream; only a BYON
// node had it stripped. Now no node holds object-storage credentials.
func TestPrepareNodeStorage_EveryNodeOnS3_GetsNoCredentials(t *testing.T) {
	storage := s3Storage(`{"bucket":"b","region":"us-east-1","accessKeyId":"AKIA_SECRET_KEY","secretAccessKey":"very-secret"}`)

	for _, node := range []*models.Node{operatorNode(), byonNode()} {
		owned := node.OwnerID != nil
		blob, objectStorage, err := PrepareNodeStorage(context.Background(), currentNodes(t), storage, node, noDeps())
		if err != nil {
			t.Fatalf("owned=%v: unexpected error: %v", owned, err)
		}
		if !objectStorage {
			t.Errorf("owned=%v: objectStorage = false, want true for an s3 row", owned)
		}
		if strings.Contains(string(blob), "AKIA_SECRET_KEY") || strings.Contains(string(blob), "very-secret") {
			t.Errorf("owned=%v: blob carries credentials: %s", owned, blob)
		}
		var stripped models.BackupStorage
		if err := json.Unmarshal(blob, &stripped); err != nil {
			t.Fatalf("unmarshal stripped blob: %v", err)
		}
		if string(stripped.Config) != "{}" {
			t.Errorf("owned=%v: stripped config = %s, want {}", owned, stripped.Config)
		}
		if stripped.Provider != "s3" || stripped.ID != storage.ID {
			t.Errorf("owned=%v: stripped blob lost non-credential fields: %+v", owned, stripped)
		}
	}
}

func TestPrepareNodeStorage_BYONNonS3Provider_ReturnsFullBlobNoURL(t *testing.T) {
	storage := &models.BackupStorage{ID: 2, Provider: "local", Config: json.RawMessage(`{"path":"/data/backups"}`)}

	// No heartbeat at all: a filesystem target involves no object storage, so
	// the version gate must not apply to it.
	blob, objectStorage, err := PrepareNodeStorage(context.Background(), heartbeatRedis(t, nil), storage, byonNode(), noDeps())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if objectStorage {
		t.Error("objectStorage = true, want false for a local provider")
	}
	want, _ := json.Marshal(storage)
	if string(blob) != string(want) {
		t.Errorf("blob = %s, want the full unstripped storage blob %s", blob, want)
	}
}

func TestPrepareNodeStorage_NilStorage_BYON_ReturnsNullNoURL(t *testing.T) {
	blob, objectStorage, err := PrepareNodeStorage(context.Background(), nil, nil, byonNode(), noDeps())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if objectStorage {
		t.Error("objectStorage = true for nil storage")
	}
	if string(blob) != "null" {
		t.Errorf("blob = %s, want null", blob)
	}
}

// FLIPPED (document F), formerly _S3OpenFails_StripsCredsButNoURL. A BYON s3 row
// Core could not open was a silent fail-safe: stripped blob, empty URL,
// dispatched anyway to fail on the node. With no credential path left there is
// nothing to fall back to, for any node, so it is an error the run is failed
// with before dispatch. Still stripped.
func TestPrepareNodeStorage_S3OpenFails_IsAnErrorAndStillStripped(t *testing.T) {
	// Missing "bucket" -> backupstorage.NewS3 returns an error.
	storage := s3Storage(`{"accessKeyId":"AKIA","secretAccessKey":"secret"}`)

	blob, _, err := PrepareNodeStorage(context.Background(), currentNodes(t), storage, byonNode(), noDeps())
	if err == nil {
		t.Fatal("no error: a run would be dispatched against a target Core cannot open")
	}
	if strings.Contains(string(blob), "secret") {
		t.Errorf("blob = %s, want credentials stripped even on failure", blob)
	}
}

// The version gate. Older, unparseable and silent nodes are refused for object
// storage; the first release with the presigned-only path, anything newer, and
// an unstamped development build pass.
// Replaces _BYONS3_OpSelectsGetVsPutSignature and _TTLThreadedFromSettings: no
// URL is signed at dispatch any more, so there is no signature or TTL left to
// pin here (the TTLs of the URLs a node asks for are pinned in
// backup_transfer_test.go).
func TestPrepareNodeStorage_RefusesANodeTooOldForPresignedTransfers(t *testing.T) {
	storage := s3Storage(`{"bucket":"b","region":"us-east-1","accessKeyId":"AKIA","secretAccessKey":"secret"}`)
	rdb := heartbeatRedis(t, map[string]string{
		"node-old":        "2026.09.14",
		"node-since":      presignedMultipartSince,
		"node-newer":      "2026.09.15",
		"node-no-version": "",
		"node-garbage":    "dev",
	})
	cases := []struct {
		token string
		want  bool
	}{
		{"node-old", false},
		{"node-since", true},
		{"node-newer", true},
		{"node-no-version", true}, // an unstamped development build
		{"node-garbage", false},
		{"node-no-heartbeat", false},
	}
	for _, c := range cases {
		t.Run(c.token, func(t *testing.T) {
			_, objectStorage, err := PrepareNodeStorage(context.Background(), rdb, storage, &models.Node{ID: 1, Token: c.token}, noDeps())
			if !objectStorage {
				t.Error("objectStorage = false, want true")
			}
			if c.want && err != nil {
				t.Errorf("err = %v, want the node accepted", err)
			}
			if !c.want && !errors.Is(err, ErrNodeUpdateRequired) {
				t.Errorf("err = %v, want ErrNodeUpdateRequired", err)
			}
		})
	}
}

// noDeps is the zero Deps: enough for s3/local, and deliberately missing the
// builders the indirection providers need, so a test that forgets to supply one
// fails the way production did.
func noDeps() backupstorage.Deps { return backupstorage.Deps{} }

func connectionStorage() *models.BackupStorage {
	return &models.BackupStorage{
		ID: 7, Name: "R2 main", Provider: "connection",
		Config: json.RawMessage(`{"connectionId":3,"prefix":"server-backups"}`),
	}
}

// The node handles exactly three providers itself, all filesystem ones. FLIPPED
// (document F): "s3" moved to the object-storage side, where Core drives the
// transfer instead of handing the node the row's credentials. Adding a provider
// to the first list without teaching the node about it sends a row the node
// answers with "unknown provider <x>" - which is how "connection" shipped broken.
func TestNodeResolvesProviderMatchesTheNodeSwitch(t *testing.T) {
	for _, p := range []string{"local", "shared", "node-local"} {
		if !nodeResolvesProvider(p) {
			t.Errorf("nodeResolvesProvider(%q) = false, want true - the node implements it", p)
		}
	}
	for _, p := range []string{"s3", "connection", "core-storage", "", "ftp"} {
		if nodeResolvesProvider(p) {
			t.Errorf("nodeResolvesProvider(%q) = true, want false - Core drives the transfer", p)
		}
	}
}

// The reported failure: an operator node got the "connection" row verbatim and
// answered "upload failed: unknown provider connection". Core resolves it for
// EVERY node. FLIPPED (document F), formerly _PresignsForOperatorNode: Core used
// to presign a PUT here at dispatch; now it proves it can open the target, and
// the node asks for URLs when the upload starts, so nothing in the command can
// expire while it waits in a queue.
func TestPrepareNodeStorage_ConnectionProvider_ResolvedForOperatorNode(t *testing.T) {
	target, err := backupstorage.NewS3(context.Background(),
		json.RawMessage(`{"bucket":"b","region":"us-east-1","accessKeyId":"AKIA","secretAccessKey":"secret"}`))
	if err != nil {
		t.Fatalf("build target: %v", err)
	}
	var gotID int
	var gotPrefix string
	deps := backupstorage.Deps{
		Connection: func(id int, prefix string) (backupstorage.Storage, error) {
			gotID, gotPrefix = id, prefix
			return target, nil
		},
	}

	blob, objectStorage, err := PrepareNodeStorage(context.Background(), currentNodes(t),
		connectionStorage(), operatorNode(), deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !objectStorage {
		t.Fatal("objectStorage = false: the node would fall back to the storage blob and fail on the provider name")
	}
	if gotID != 3 || gotPrefix != "server-backups" {
		t.Errorf("resolved connection %d prefix %q, want 3 / server-backups", gotID, gotPrefix)
	}
	var stripped models.BackupStorage
	if err := json.Unmarshal(blob, &stripped); err != nil {
		t.Fatalf("unmarshal blob: %v", err)
	}
	if string(stripped.Config) != "{}" {
		t.Errorf("config = %s, want {} - the node has no use for it", stripped.Config)
	}
}

// An unresolvable indirection has no node-side fallback, so it must surface as
// an error the caller can fail the run with, naming the storage row.
func TestPrepareNodeStorage_IndirectProviderWithoutBuilder_Errors(t *testing.T) {
	for _, tc := range []struct{ name, provider string }{
		{"connection", "connection"},
		{"core storage", "core-storage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := connectionStorage()
			s.Provider = tc.provider
			_, _, err := PrepareNodeStorage(context.Background(), currentNodes(t),
				s, operatorNode(), noDeps())
			if err == nil {
				t.Fatal("no error: a run would be dispatched that the node cannot execute")
			}
			if !strings.Contains(err.Error(), "R2 main") || !strings.Contains(err.Error(), tc.provider) {
				t.Errorf("error %q names neither the storage row nor the provider", err)
			}
		})
	}
}
