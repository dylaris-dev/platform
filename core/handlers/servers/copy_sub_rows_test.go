package servers

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	pb "dylaris-proto/node"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
)

// emptyRootNode answers every listing with an empty server root.
type emptyRootNode struct {
	grpc.ServerStream
	conn *nodegrpc.NodeConnection
}

func (s *emptyRootNode) Context() context.Context       { return context.Background() }
func (s *emptyRootNode) Recv() (*pb.NodeMessage, error) { select {} }
func (s *emptyRootNode) Send(m *pb.NodeMessage) error {
	go s.conn.RouteResponse(&pb.NodeMessage{RequestId: m.RequestId,
		Payload: &pb.NodeMessage_ListResp{ListResp: &pb.ListFilesResp{}}})
	return nil
}

// copyRowsStore records the copy target's rows and whether the command was
// already queued when each was written.
type copyRowsStore struct {
	store.Store
	srv          models.Server
	queue        *redis.Client
	afterSend    int // rows written once copy_sub_server was in the stream
	mods         int // target mod rows that exist
	install      bool
	contents     int
	modsCleared  bool
	installGone  bool
	contentsGone bool
}

func (f *copyRowsStore) sent() {
	if n, _ := f.queue.XLen(context.Background(), "dylaris:node:tok:cmds").Result(); n > 0 {
		f.afterSend++
	}
}
func (f *copyRowsStore) GetServerByID(int) (*models.Server, error) { s := f.srv; return &s, nil }
func (f *copyRowsStore) GetNodeByID(int) (*models.Node, error) {
	return &models.Node{ID: 7, Token: "tok"}, nil
}
func (f *copyRowsStore) GetSetting(string) (string, error) { return "", nil }
func (f *copyRowsStore) ListServerMods(int, string) ([]models.ServerMod, error) {
	return []models.ServerMod{{ServerID: 1, SubServerName: "main", Title: "A", FileName: "a.jar", Status: models.ServerModInstalled}}, nil
}
func (f *copyRowsStore) UpsertServerMod(*models.ServerMod) (int, error) {
	f.sent()
	f.mods++
	return 1, nil
}
func (f *copyRowsStore) GetSubServerInstall(int, string) (*models.SubServerInstall, error) {
	return &models.SubServerInstall{ServerID: 1, SubServerName: "main", InstallerType: "fabric"}, nil
}
func (f *copyRowsStore) UpsertSubServerInstall(models.SubServerInstall) error {
	f.sent()
	f.install = true
	return nil
}
func (f *copyRowsStore) GetModpackScan(int, string) (string, int, int, error) { return "", 0, 0, nil }
func (f *copyRowsStore) ListServerModpackContents(int, string) ([]models.ServerModpackContent, error) {
	return []models.ServerModpackContent{{ServerID: 1, SubServerName: "main"}}, nil
}
func (f *copyRowsStore) ReplaceServerModpackContents(_ int, _ string, rows []models.ServerModpackContent) error {
	if rows == nil {
		f.contentsGone = true
		return nil
	}
	f.sent()
	f.contents = len(rows)
	return nil
}
func (f *copyRowsStore) ReplaceServerMods(_ int, _ string, rows []models.ServerMod) error {
	f.modsCleared = rows == nil
	return nil
}
func (f *copyRowsStore) DeleteSubServerInstall(int, string) error { f.installGone = true; return nil }

// The node reports a copy it cannot make within milliseconds, and the status
// watcher removes the target's rows when it sees that report. Rows written
// after the dispatch could land after that removal and stay for good, so they
// are written first, and a dispatch that never reached the queue takes them
// back out.
func TestACopyWritesItsRowsBeforeTheDispatch(t *testing.T) {
	for _, c := range []struct {
		name     string
		queueDie bool
	}{
		{"queued", false},
		{"queue unreachable", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			queueRdb := rdb
			if c.queueDie {
				dead := miniredis.RunT(t)
				queueRdb = redis.NewClient(&redis.Options{Addr: dead.Addr()})
				dead.Close()
			}
			fs := &copyRowsStore{srv: models.Server{ID: 1, UUID: "srv-1", NodeID: 7, ActiveSubServer: "main", Status: "stopped"}, queue: rdb}
			reg := nodegrpc.NewRegistry()
			node := &emptyRootNode{}
			node.conn = reg.Register(7, "tok", node)
			h := NewServerModsHandler(testDeps(Deps{Store: fs, Redis: rdb, GRPCRegistry: reg, Queue: services.NewQueueService(queueRdb)}))
			r := httptest.NewRequest("POST", "/api/servers/1/copy-sub-server", bytes.NewBufferString(`{"targetName":"copy"}`))
			r = mux.SetURLVars(r.WithContext(context.WithValue(r.Context(), "isAdmin", false)), map[string]string{"id": "1"})
			rec := httptest.NewRecorder()
			h.CopySubServer(rec, r)

			if c.queueDie {
				if rec.Code != 500 {
					t.Fatalf("answered %d %s, want 500", rec.Code, rec.Body.String())
				}
				if fs.mods > 0 && !fs.modsCleared || fs.install && !fs.installGone || fs.contents > 0 && !fs.contentsGone {
					t.Errorf("rows left for a copy that was never queued: %+v", fs)
				}
				return
			}
			if rec.Code != 200 {
				t.Fatalf("answered %d %s", rec.Code, rec.Body.String())
			}
			if fs.mods != 1 || !fs.install || fs.contents != 1 {
				t.Fatalf("target rows not written: %+v", fs)
			}
			if fs.afterSend != 0 {
				t.Errorf("%d rows written after copy_sub_server was queued", fs.afterSend)
			}
		})
	}
}
