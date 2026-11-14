package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// D1 (7b-2 E2E): a moved server is started on its new node by the reconciler,
// which rebuilds the missing container from the saved config. That start wrote
// no key.pem, so the key copied with the files won and Bedrock players were
// refused until the next restart. Every start writes the key now.
func TestAMovedServerGetsItsFloodgateKeyAtStart(t *testing.T) {
	const keyFile = "main/plugins/floodgate/key.pem"
	stale := []byte("the key the move copied")
	tests := []struct {
		name      string
		published string // "" = no key in Redis
		redisDown bool
		layout    string // directory in the sub-server that marks Floodgate's layout
		want      []byte // nil = the stale file is left as it was
	}{
		{"key in redis replaces the copied one", b64(testFloodgateKey), false, "plugins/floodgate", testFloodgateKey},
		{"no key in redis leaves the file alone", "", false, "plugins/floodgate", nil},
		{"redis unreachable still starts", b64(testFloodgateKey), true, "plugins/floodgate", nil},
		{"java-only server gets nothing", b64(testFloodgateKey), false, "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			mr.Set(desiredStateKey(sharedDaemonUUID), "online")
			if tc.published != "" {
				mr.Set("dylaris:server:"+sharedDaemonUUID+":floodgate", tc.published)
			}

			root := t.TempDir()
			serverDir := filepath.Join(root, sharedDaemonUUID)
			if err := os.MkdirAll(filepath.Join(serverDir, "main"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.layout != "" {
				p := filepath.Join(serverDir, keyFile)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, stale, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(serverDir, ".node_config.json"),
				[]byte(`{"uuid":"`+sharedDaemonUUID+`","activeSubServer":"main","docker":{"image":"img:1","ram":1024}}`), 0o600); err != nil {
				t.Fatal(err)
			}

			// The target of a move: no container yet.
			dm, d := newSharedDaemon(t, "-", false, nil)
			storage := &StorageManager{paths: []string{root}}
			dm.storageMgr = storage
			dm.floodgateRDB = rdb
			if tc.redisDown {
				dead := miniredis.RunT(t)
				dm.floodgateRDB = redis.NewClient(&redis.Options{Addr: dead.Addr()})
				dead.Close()
			}

			reconcileDeletedContainers(t.Context(), rdb, dm, storage)

			if !slices.Contains(d.changed(), "start new-id-0123456789ab") {
				t.Fatalf("the moved server was not started: %v", d.changed())
			}
			p := filepath.Join(serverDir, filepath.FromSlash(keyFile))
			got, err := os.ReadFile(p)
			if tc.layout == "" {
				if err == nil {
					t.Fatal("a key file was written for a server without Floodgate")
				}
				return
			}
			if err != nil {
				t.Fatalf("key file: %v", err)
			}
			want := tc.want
			if want == nil {
				want = stale
			}
			// Compared, never printed: the key is a secret.
			if !bytes.Equal(got, want) {
				t.Errorf("key file holds %d bytes that are not the expected %d (published key written: %v)", len(got), len(want), tc.want != nil)
			}
			if tc.want != nil && runtime.GOOS != "windows" {
				if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != nodeOwnedFileMode {
					t.Errorf("key file mode = %v, %v; want %v", fi.Mode().Perm(), err, nodeOwnedFileMode)
				}
			}
		})
	}
}

// The reconciler's crash restart starts an existing container as it is, and so
// does every exited server after a host reboot, and a server left stopped by a
// setup or a reconfigure is first started that way. The key must never decide
// whether that start happens.
func TestCrashRestartWritesTheFloodgateKey(t *testing.T) {
	prev := nodeRedis
	nodeRedis = &redisAddrState{}
	nodeRedis.set("10.0.0.9:6379", false)
	t.Cleanup(func() { nodeRedis = prev })

	const keyRel = "main/plugins/floodgate/key.pem"
	tests := []struct {
		name      string
		floodgate bool // the sub-server has Floodgate's folder with an old key in it
		redisDown bool
		written   bool // the published key replaces the old one
	}{
		{"key in redis is written", true, false, true},
		{"redis unreachable still starts", true, true, false},
		{"java-only server gets nothing", false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			mr.Set("dylaris:server:"+sharedDaemonUUID+":floodgate", b64(testFloodgateKey))
			dm, d := newSharedDaemon(t, "node-a", false, nil)
			d.env = buildRedisEnv(sharedDaemonUUID, "", "10.0.0.9:6379") // current: started, not recreated
			dm.floodgateRDB = redis.NewClient(&redis.Options{Addr: mr.Addr()})
			if tc.redisDown {
				mr.Close()
			}
			serverDir := filepath.Join(dm.localDataPath, "servers", sharedDaemonUUID)
			files := map[string]string{"main/server.properties": "motd=x"}
			if tc.floodgate {
				files[keyRel] = "stale"
			}
			for rel, body := range files {
				p := filepath.Join(serverDir, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var logged bytes.Buffer
			log.SetOutput(&logged)
			err := dm.PowerAction(sharedDaemonUUID, "start")
			log.SetOutput(os.Stderr)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			if ch := d.changed(); !slices.Equal(ch, []string{"start c-id"}) {
				t.Fatalf("changes = %v, want a plain start", ch)
			}
			if got := strings.Contains(logged.String(), "floodgate key: not read, starting without it"); got != tc.redisDown {
				t.Errorf("key read failure logged = %v, want %v", got, tc.redisDown)
			}

			want := files
			if tc.written {
				want[keyRel] = string(testFloodgateKey)
			}
			// The sub-server only: the start's network gate keeps a marker of
			// its own in the server root.
			got := map[string]string{}
			filepath.WalkDir(filepath.Join(serverDir, "main"), func(p string, e os.DirEntry, err error) error {
				if err == nil && !e.IsDir() {
					rel, _ := filepath.Rel(serverDir, p)
					b, _ := os.ReadFile(p)
					got[filepath.ToSlash(rel)] = string(b)
				}
				return nil
			})
			// Compared, never printed: one of the values is the key.
			for rel := range got {
				if _, ok := want[rel]; !ok {
					t.Errorf("%s was created", rel)
				}
			}
			for rel, body := range want {
				if g, ok := got[rel]; !ok {
					t.Errorf("%s was deleted", rel)
				} else if g != body {
					t.Errorf("%s holds %d bytes, not the expected %d", rel, len(g), len(body))
				}
			}
		})
	}
}

// Without this line in main the start writes nothing in production, and every
// test above still passes.
func TestMainHandsTheStartItsRedis(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("dockerMgr.floodgateRDB = rdb")) {
		t.Error("main no longer sets dockerMgr.floodgateRDB; a start would not write the Floodgate key")
	}
}
