package guard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/services"
	pb "dylaris-proto/node"
)

// A copy waiting in the node's stream holds no node_busy yet, so every sibling
// of the version move (setup, reinstall, switch, restore, delete, a start) ran
// beside it and the copy captured a tree being rewritten. A copy that has
// landed lets them through and is forgotten.
func TestTheBusyGuardWaitsForAQueuedCopy(t *testing.T) {
	srv := &models.Server{UUID: "srv-c", NodeID: 3, ActiveSubServer: "main", Status: "stopped"}
	for _, c := range []struct {
		name       string
		pending    string
		entries    []*pb.FileInfo
		listErr    error
		code       int // 0: not refused
		keyCleared bool
	}{
		{"no copy queued", "", nil, nil, 0, true},
		{"queued, not landed", "main-old", []*pb.FileInfo{{Name: "main", IsDir: true}}, nil, http.StatusConflict, false},
		{"landed", "main-old", []*pb.FileInfo{{Name: "main", IsDir: true}, {Name: "main-old", IsDir: true}}, nil, 0, true},
		{"node does not answer", "main-old", nil, errors.New("node 3 not connected"), http.StatusBadGateway, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rdb, _ := handlertest.Redis(t)
			if c.pending != "" {
				rdb.Set(context.Background(), services.CopyPendingKey(srv.UUID), c.pending, 0)
			}
			old := listRootNames
			listRootNames = func(*nodegrpc.Registry, int, string) ([]*pb.FileInfo, error) { return c.entries, c.listErr }
			defer func() { listRootNames = old }()

			rec := httptest.NewRecorder()
			got := RefuseIfServerBusy(rec, httptest.NewRequest(http.MethodPost, "/", nil), rdb, nil, srv)
			if got != (c.code != 0) || (c.code != 0 && rec.Code != c.code) {
				t.Fatalf("refused=%v code=%d, want code %d", got, rec.Code, c.code)
			}
			if c.code == http.StatusConflict && !strings.Contains(rec.Body.String(), "copy_running") {
				t.Errorf("body %s, want the copy_running code the panel waits on", rec.Body.String())
			}
			n, _ := rdb.Exists(context.Background(), services.CopyPendingKey(srv.UUID)).Result()
			if (n == 0) != c.keyCleared {
				t.Errorf("copy_pending exists=%d, want cleared=%v", n, c.keyCleared)
			}
		})
	}
}
