package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/services"
)

// transferStore is the owner's own transfer, with the server's status and the
// source node's state set per case.
type transferStore struct {
	*twoNodeFakeStore
	status   string
	onTarget int // servers already on the target node
}

func (f *transferStore) CountServersByNode(int) (int, error) { return f.onTarget, nil }

func (f *transferStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-1", OwnerID: visOwner, NodeID: f.node.ID, Status: f.status}, nil
}

func transferAs(t *testing.T, st *transferStore) *httptest.ResponseRecorder {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	s := &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st),
		Migration: services.NewMigrationOrchestrator(st, rdb, services.NewQueueService(rdb), nil, "x")}
	h := &ServerHandler{state: s}
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/x", bytes.NewBufferString(`{"targetNodeId":8}`))
		ctx := context.WithValue(r.Context(), "isAdmin", false)
		ctx = context.WithValue(ctx, "userID", visOwner)
		r = mux.SetURLVars(r.WithContext(ctx), map[string]string{"id": "3"})
		rw := httptest.NewRecorder()
		h.TransferServer(rw, r)
		return rw
	}
	first := call()
	if first.Code != http.StatusAccepted {
		return first
	}
	return call()
}

func newTransferStore(sourceStatus, serverStatus string) *transferStore {
	owner := visOwner
	return &transferStore{
		twoNodeFakeStore: &twoNodeFakeStore{
			foreignFakeStore: foreignFakeStore{node: &models.Node{ID: 7, Status: sourceStatus}, byon: "true"},
			target:           &models.Node{ID: 8, OwnerID: &owner, Status: "online"},
		},
		status: serverStatus,
	}
}

// A suspended server is not the owner's to move: the move ended in "stopped",
// which the power gate does not refuse.
func TestATransferOfASuspendedServerIsRefused(t *testing.T) {
	rw := transferAs(t, newTransferStore("online", "suspended"))
	if rw.Code != http.StatusForbidden || !bytes.Contains(rw.Body.Bytes(), []byte("suspended")) {
		t.Fatalf("status %d: %s", rw.Code, rw.Body)
	}
}

// A source that is not there cannot be moved from; waiting for it held the
// platform's one migration slot while the server sat stopped.
func TestATransferFromAnOfflineNodeIsRefused(t *testing.T) {
	rw := transferAs(t, newTransferStore("offline", "stopped"))
	if rw.Code != http.StatusConflict || !bytes.Contains(rw.Body.Bytes(), []byte("offline")) {
		t.Fatalf("status %d: %s", rw.Code, rw.Body)
	}
}

// One request per server: repeating a transfer held the queue for everyone.
// transferAs sends it twice; the second is refused.
func TestATransferRepeatedWhileQueuedIsRefused(t *testing.T) {
	rw := transferAs(t, newTransferStore("online", "stopped"))
	if rw.Code != http.StatusConflict || !bytes.Contains(rw.Body.Bytes(), []byte("already queued")) {
		t.Fatalf("status %d: %s", rw.Code, rw.Body)
	}
}

// The per-node cap CreateServer applies: creating servers on one node and
// moving them onto another filled that one past it.
func TestATransferOntoAFullNodeIsRefused(t *testing.T) {
	full := newTransferStore("online", "stopped")
	full.onTarget = byonNodeServerFallbackCap
	if rw := transferAs(t, full); rw.Code != http.StatusForbidden || !bytes.Contains(rw.Body.Bytes(), []byte("server limit")) {
		t.Fatalf("full target: status %d: %s", rw.Code, rw.Body)
	}
	room := newTransferStore("online", "stopped")
	room.onTarget = byonNodeServerFallbackCap - 1
	// transferAs sends twice: the first is accepted, so the second meets the queue.
	if rw := transferAs(t, room); !bytes.Contains(rw.Body.Bytes(), []byte("already queued")) {
		t.Fatalf("a target with room was refused: %d %s", rw.Code, rw.Body)
	}
}
