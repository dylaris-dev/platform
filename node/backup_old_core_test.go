package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"

	"dylaris-pkg/queue"
	pb "dylaris-proto/node"
)

// A node no longer has code that could use bucket credentials or a URL minted
// at dispatch. What an older Core sends for object storage - credentials in the
// storage blob or a presigned URL in the command, and never upload "multipart"
// or download "presigned" - must fail at once and say why, without reaching for
// the network, and for a restore without stopping the server.

const oldCoreMessage = "older than this node, or the command is invalid"

// hitCounter is an HTTP endpoint that counts every request it gets.
func hitCounter(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// countCoreRequests replaces the control stream with one that refuses and
// counts every request.
func countCoreRequests(t *testing.T) *atomic.Int32 {
	t.Helper()
	var asked atomic.Int32
	prevNode, prevReq := nodeID, coreRequest
	t.Cleanup(func() { nodeID, coreRequest = prevNode, prevReq })
	nodeID = "node-old-core"
	coreRequest = func(context.Context, *pb.NodeMessage, time.Duration) (*pb.NodeMessage, error) {
		asked.Add(1)
		return nil, errNoCoreConnection
	}
	return &asked
}

// terminalReport runs fn and returns the first report on channel that is not
// a progress report.
func terminalReport(t *testing.T, rdb *redis.Client, channel string, fn func()) map[string]interface{} {
	t.Helper()
	sub := rdb.Subscribe(context.Background(), channel)
	t.Cleanup(func() { sub.Close() })
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	fn()
	for {
		msg, err := sub.ReceiveTimeout(context.Background(), 5*time.Second)
		if err != nil {
			t.Fatalf("no report: %v", err)
		}
		m, ok := msg.(*redis.Message)
		if !ok {
			continue
		}
		var report map[string]interface{}
		_ = json.Unmarshal([]byte(m.Payload), &report)
		if report["status"] != "running" {
			return report
		}
	}
}

// oldCoreCommand decodes a command the way the queue consumer does, from the
// JSON an older Core sent: credentials in the blob, a URL in the command.
func oldCoreCommand(t *testing.T, provider, endpoint string, fields map[string]interface{}, into interface{}) {
	t.Helper()
	cfg, _ := json.Marshal(map[string]interface{}{
		"endpoint": endpoint, "bucket": "b", "region": "us-east-1", "forcePathStyle": true,
		"accessKeyId": "AKIA-OLD-CORE", "secretAccessKey": "old-core-secret",
	})
	fields["storage"] = map[string]interface{}{"id": 4, "provider": provider, "config": json.RawMessage(cfg)}
	raw, _ := json.Marshal(fields)
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

func newMiniRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

func TestRunBackup_ObjectStorageWithoutMultipartFailsWithoutTheNetwork(t *testing.T) {
	for _, provider := range []string{"s3", "connection", "core-storage", "unknown-kind"} {
		t.Run(provider, func(t *testing.T) {
			bucket, hits := hitCounter(t)
			asked := countCoreRequests(t)
			root := t.TempDir()
			const uuid = "srv-old-core"
			if err := os.MkdirAll(filepath.Join(root, uuid, "world"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, uuid, "world", "level.dat"), []byte("data"), 0o644); err != nil {
				t.Fatal(err)
			}
			rdb := newMiniRedis(t)

			var cmd BackupRunCommand
			oldCoreCommand(t, provider, bucket.URL, map[string]interface{}{
				"runId": 41, "jobId": 1, "serverUuid": uuid, "storageKey": "backups/x.tar.gz",
				"presignedPutUrl": bucket.URL + "/b/x.tar.gz",
			}, &cmd)

			report := terminalReport(t, rdb, queue.BackupResultsChannel(nodeID), func() {
				RunBackup(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
			})
			if report["status"] != "failed" || !strings.Contains(report["error"].(string), oldCoreMessage) {
				t.Fatalf("report = %v, want a failure naming an old Core or an invalid command", report)
			}
			if strings.Contains(report["error"].(string), "old-core-secret") {
				t.Fatal("the report carries the credential")
			}
			if hits.Load() != 0 || asked.Load() != 0 {
				t.Fatalf("storage got %d requests and Core %d, want none", hits.Load(), asked.Load())
			}
		})
	}
}

// fakeDocker is a Docker API that counts every call. Any call at all during a
// refused restore means the node went for the container.
func fakeDocker(t *testing.T) (*DockerManager, *atomic.Int32) {
	t.Helper()
	srv, hits := hitCounter(t)
	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")),
		client.WithVersion("1.44"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return &DockerManager{cli: cli, ctx: t.Context()}, hits
}

func TestRunRestore_ObjectStorageWithoutPresignedFailsBeforeTheServerStops(t *testing.T) {
	for _, provider := range []string{"s3", "connection", "core-storage"} {
		t.Run(provider, func(t *testing.T) {
			bucket, hits := hitCounter(t)
			asked := countCoreRequests(t)
			dm, dockerCalls := fakeDocker(t)
			root := t.TempDir()
			const uuid = "srv-old-core-restore"
			if err := os.MkdirAll(filepath.Join(root, uuid), 0o755); err != nil {
				t.Fatal(err)
			}
			rdb := newMiniRedis(t)

			var cmd BackupRestoreCommand
			oldCoreCommand(t, provider, bucket.URL, map[string]interface{}{
				"runId": 9, "restoreId": 52, "jobId": 1, "serverUuid": uuid, "storageKey": "backups/x.tar.gz",
				"presignedGetUrl": bucket.URL + "/b/x.tar.gz",
			}, &cmd)

			report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
				RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, dm, cmd)
			})
			if report["status"] != "failed" || !strings.Contains(report["error"].(string), oldCoreMessage) {
				t.Fatalf("report = %v, want a failure naming an old Core or an invalid command", report)
			}
			if dockerCalls.Load() != 0 {
				t.Fatalf("the Docker API was called %d times: the server was stopped for a restore that could not run", dockerCalls.Load())
			}
			if hits.Load() != 0 || asked.Load() != 0 {
				t.Fatalf("storage got %d requests and Core %d, want none", hits.Load(), asked.Load())
			}
			if entries, _ := os.ReadDir(root); len(entries) != 1 {
				t.Fatalf("the restore left %d entries beside the server directory", len(entries)-1)
			}
		})
	}
}

// The control for the test above: the same fake Docker DOES see the stop when
// the restore is one the node can run, so zero calls there means something. A
// filesystem provider needs no mode, which is also the part of downloadBackup
// that stays.
func TestRunRestore_FilesystemRestoreStillStopsAndRestores(t *testing.T) {
	countCoreRequests(t)
	dm, dockerCalls := fakeDocker(t)
	root, base := t.TempDir(), t.TempDir()
	const uuid, key = "srv-local-restore", "backups/srv/job-1/run.tar.gz"
	if err := os.MkdirAll(filepath.Join(root, uuid), 0o755); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	gw := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: "world/level.dat", Mode: 0o644, Size: 8, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("RESTORED"))
	_ = tw.Close()
	_ = gw.Close()
	full := filepath.Join(base, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, archive.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(localCfg{BasePath: base})
	blob, _ := json.Marshal(storageInfo{ID: 1, Provider: "local", Config: cfg})
	rdb := newMiniRedis(t)

	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, dm, BackupRestoreCommand{
			RunID: 9, RestoreID: 53, ServerUUID: uuid, StorageKey: key, Storage: blob,
		})
	})
	if report["status"] != "success" {
		t.Fatalf("report = %v, want success", report)
	}
	if dockerCalls.Load() == 0 {
		t.Fatal("the fake Docker saw no call, so it cannot show that a refused restore made none")
	}
	if got, err := os.ReadFile(filepath.Join(root, uuid, "world", "level.dat")); err != nil || string(got) != "RESTORED" {
		t.Fatalf("restored file = %q, %v", got, err)
	}
}
