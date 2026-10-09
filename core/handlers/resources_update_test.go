package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// resourcesFakeStore embeds store.Store (nil); only what UpdateServerResources
// touches is overridden, and it records every write so a test can tell a
// refused request from a half-applied one.
type resourcesFakeStore struct {
	store.Store

	server *models.Server

	resourcesWritten bool
	pinningWritten   bool
	portsWritten     bool
}

func (f *resourcesFakeStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, Role: "admin"}, nil
}
func (f *resourcesFakeStore) GetUserRegionIDs(string) ([]string, error) { return nil, nil }
func (f *resourcesFakeStore) GetServerByID(int) (*models.Server, error) {
	s := *f.server
	return &s, nil
}
func (f *resourcesFakeStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id, Token: "node-tok-7"}, nil
}
func (f *resourcesFakeStore) UpdateServerResources(int, int, float64, int64) error {
	f.resourcesWritten = true
	return nil
}
func (f *resourcesFakeStore) UpdateServerCPUPinning(int, string, string) error {
	f.pinningWritten = true
	return nil
}
func (f *resourcesFakeStore) GetUsedHostPortsOnNode(int) ([]int, error) { return nil, nil }
func (f *resourcesFakeStore) UpdateServerPorts(int, int, int) error {
	f.portsWritten = true
	return nil
}
func (f *resourcesFakeStore) GetServerAuditState(int) (bool, bool, int, error) {
	return false, false, 0, nil
}

func resourcesReq(t *testing.T, body map[string]any) *http.Request {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPatch, "/api/servers/1/resources", bytes.NewReader(b))
	r = mux.SetURLVars(r, map[string]string{"id": "1"})
	ctx := context.WithValue(r.Context(), "isAdmin", true)
	ctx = context.WithValue(ctx, "userID", "admin-1")
	return r.WithContext(ctx)
}

func resourcesServer(status string) *models.Server {
	return &models.Server{ID: 1, UUID: "srv-1", NodeID: 7, Status: status, HostPort: 25600, ContainerPort: 25565, CPUPinningMode: "shared"}
}

// A cpuset refused AFTER the RAM was saved left the server half-changed: the
// panel showed the new RAM, the node never heard of it. Validation runs first.
func TestUpdateServerResources_InvalidCpusetWritesNothing(t *testing.T) {
	fake := &resourcesFakeStore{server: resourcesServer("online")}
	h := &ServerHandler{state: &AppState{Store: fake, CPUPinning: &services.CPUPinningService{}}}

	rec := httptest.NewRecorder()
	h.UpdateServerResources(rec, resourcesReq(t, map[string]any{
		"ram": 4096, "cpuLimit": 2.0, "diskLimit": 10240, "cpuPinningMode": "manual", "cpuset": "",
	}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if fake.resourcesWritten || fake.pinningWritten || fake.portsWritten {
		t.Errorf("a refused request still wrote: resources=%v pinning=%v ports=%v", fake.resourcesWritten, fake.pinningWritten, fake.portsWritten)
	}
}

func TestUpdateServerResources_RefusedWhileInstallingOrMigrating(t *testing.T) {
	for _, status := range []string{"installing", "migrating"} {
		t.Run(status, func(t *testing.T) {
			fake := &resourcesFakeStore{server: resourcesServer(status)}
			h := &ServerHandler{state: &AppState{Store: fake}}

			rec := httptest.NewRecorder()
			h.UpdateServerResources(rec, resourcesReq(t, map[string]any{"ram": 4096, "cpuLimit": 2.0, "diskLimit": 10240}))

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
			}
			if fake.resourcesWritten {
				t.Error("resources were written for a server that is " + status)
			}
		})
	}
}

func resourcesRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

// A container-port-only change was saved and never sent: the change detection
// compared the host port alone, so the container kept listening where it was.
func TestUpdateServerResources_ContainerPortOnlyChangeReachesTheNode(t *testing.T) {
	rdb := resourcesRedis(t)
	fake := &resourcesFakeStore{server: resourcesServer("online")}
	h := &ServerHandler{state: &AppState{Store: fake, Queue: services.NewQueueService(rdb)}}

	rec := httptest.NewRecorder()
	h.UpdateServerResources(rec, resourcesReq(t, map[string]any{
		"ram": 4096, "cpuLimit": 2.0, "diskLimit": 10240, "containerPort": 25570,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	cmd := readNodeCmdStream(t, rdb, "dylaris:node:node-tok-7:cmds")
	cfg, _ := cmd["config"].(map[string]interface{})
	docker, _ := cfg["docker"].(map[string]interface{})
	if got, _ := docker["containerPort"].(float64); got != 25570 {
		t.Errorf("payload containerPort = %v, want 25570 (docker=%v)", docker["containerPort"], docker)
	}
	if got, _ := docker["hostPort"].(float64); got != 25600 {
		t.Errorf("payload hostPort = %v, want the unchanged 25600", docker["hostPort"])
	}
	if _, ok := docker["command"]; ok {
		t.Errorf("payload carries a start command %q; the node builds its own", docker["command"])
	}
}

// The command used to be fire-and-forget: a failed queue answered success and
// the panel showed values the container never got.
func TestUpdateServerResources_DispatchFailureIsReported(t *testing.T) {
	brokenRDB := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 300 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { brokenRDB.Close() })

	fake := &resourcesFakeStore{server: resourcesServer("online")}
	h := &ServerHandler{state: &AppState{Store: fake, Queue: services.NewQueueService(brokenRDB)}}

	rec := httptest.NewRecorder()
	h.UpdateServerResources(rec, resourcesReq(t, map[string]any{"ram": 4096, "cpuLimit": 2.0, "diskLimit": 10240}))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("saved")) {
		t.Errorf("the error does not say the values were saved: %s", rec.Body.String())
	}
}

// "Save again" after a failed dispatch finds the ports already in the row. If
// only a difference from the row sent them, the port change never reached the
// node at all.
func TestUpdateServerResources_RequestedPortsAreSentEvenWhenUnchanged(t *testing.T) {
	rdb := resourcesRedis(t)
	fake := &resourcesFakeStore{server: resourcesServer("online")}
	h := &ServerHandler{state: &AppState{Store: fake, Queue: services.NewQueueService(rdb)}}

	rec := httptest.NewRecorder()
	h.UpdateServerResources(rec, resourcesReq(t, map[string]any{
		"ram": 4096, "cpuLimit": 2.0, "diskLimit": 10240, "hostPort": 25600, "containerPort": 25565,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	cmd := readNodeCmdStream(t, rdb, "dylaris:node:node-tok-7:cmds")
	cfg, _ := cmd["config"].(map[string]interface{})
	docker, _ := cfg["docker"].(map[string]interface{})
	if docker["hostPort"] != float64(25600) || docker["containerPort"] != float64(25565) {
		t.Errorf("payload ports = %v/%v, want 25600/25565", docker["hostPort"], docker["containerPort"])
	}
}
