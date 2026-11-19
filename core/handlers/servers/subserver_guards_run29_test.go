package servers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/handlers/guard"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/services"

	pb "dylaris-proto/node"

	"github.com/gorilla/mux"
	"google.golang.org/grpc"
)

// rootNode answers every request with a server root holding dirs.
type rootNode struct {
	grpc.ServerStream
	conn *nodegrpc.NodeConnection
	dirs []string
}

func (s *rootNode) Context() context.Context       { return context.Background() }
func (s *rootNode) Recv() (*pb.NodeMessage, error) { select {} }
func (s *rootNode) Send(m *pb.NodeMessage) error {
	files := []*pb.FileInfo{}
	for _, d := range s.dirs {
		files = append(files, &pb.FileInfo{Name: d, IsDir: true})
	}
	go s.conn.RouteResponse(&pb.NodeMessage{RequestId: m.RequestId,
		Payload: &pb.NodeMessage_ListResp{ListResp: &pb.ListFilesResp{Files: files}}})
	return nil
}

// R29-01: the cap was skipped whenever the row read pending_setup, and a
// failed setup or a deleted active slot leaves that status over sub-server
// directories that are still on disk. The node lists a missing server dir as
// empty, so a truly fresh server passes on the count alone.
func TestSetupCountsSubServersWhilePendingSetup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		limit   string
		onDisk  []string
		refused bool
	}{
		{"fresh server, nothing on disk", "1", nil, false},
		// The first setup was never refused, a cap of none included: the
		// server itself is what was bought.
		{"fresh server, a cap of none", "0", nil, false},
		{"pending_setup over a leftover sub-server", "1", []string{"old"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fs, rdb := newInstallTest(t)
			fs.settings[guard.SettingMaxSubServers] = tc.limit
			reg := nodegrpc.NewRegistry()
			node := &rootNode{dirs: tc.onDisk}
			node.conn = reg.Register(7, "node-lib", node)
			h.d.GRPCRegistry = reg
			rec := httptest.NewRecorder()
			h.SetupServer(rec, installRequest("setup", setupBody(map[string]string{"type": "paper", "version": "1.21.1"}), true))
			refused := rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "Sub-server limit reached")
			if refused != tc.refused {
				t.Fatalf("answered %d %s, want refused=%v", rec.Code, rec.Body.String(), tc.refused)
			}
			if tc.refused && len(queuedInstaller(t, rdb)) > 0 {
				t.Fatal("the refused setup was queued for the node")
			}
		})
	}
}

// R29-03: a setup queued but not yet started holds no busy key on the node,
// and a switch let through beside it left Core on one sub-server and the node
// on the other. Refused like setup, reinstall and delete.
func TestSwitchRefusesAServerBeingInstalled(t *testing.T) {
	h, fs, rdb := newInstallTest(t)
	fs.server.Status = "installing"
	rec := httptest.NewRecorder()
	h.SwitchSubServer(rec, installRequest("switch", map[string]any{"subServerName": "main"}, true))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "server_installing") {
		t.Fatalf("answered %d %s, want 409 server_installing", rec.Code, rec.Body.String())
	}
	if n, _ := rdb.XLen(context.Background(), "dylaris:node:node-lib:cmds").Result(); n != 0 {
		t.Fatalf("%d command(s) queued for a refused switch", n)
	}
}

// R29-02: deleting the active slot during a start or restart let the node
// rebuild the container on the deleted slot. Refused like CopySubServer, and
// the "stopped" it writes reaches the node at once, not on the next scan.
func TestDeleteSubServerDuringAPowerTransition(t *testing.T) {
	for _, tc := range []struct {
		status, sub string
		wantStatus  int
	}{
		{"starting", "survival", http.StatusConflict},
		{"stopping", "survival", http.StatusConflict},
		{"restarting", "survival", http.StatusConflict},
		{"starting", "creative", http.StatusOK}, // not the slot being started
		{"stopped", "survival", http.StatusOK},
	} {
		t.Run(tc.status+"/"+tc.sub, func(t *testing.T) {
			rdb := handlertest.RedisClient(t)
			fs := &subServerDeleteFakeStore{admin: true, status: tc.status}
			h := &ServerHandler{d: testDeps(Deps{Store: fs, Redis: rdb, Events: services.NewSystemEventsPublisher(nil)})}
			req := httptest.NewRequest(http.MethodDelete, "/api/servers/7/sub-servers/"+tc.sub, nil)
			ctx := context.WithValue(req.Context(), "userID", "alice")
			ctx = context.WithValue(ctx, "isAdmin", true)
			req = mux.SetURLVars(req.WithContext(ctx), map[string]string{"id": "7", "subServerName": tc.sub})
			rw := httptest.NewRecorder()
			h.DeleteSubServer(rw, req)
			if rw.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rw.Code, tc.wantStatus, rw.Body.String())
			}
			desired, _ := rdb.Get(context.Background(), services.DesiredStateKey("srv-uuid")).Result()
			if tc.wantStatus == http.StatusConflict {
				if fs.statusSet != "" || fs.installCleared != "" {
					t.Error("the refused delete still reset the row or dropped the install record")
				}
				return
			}
			if wantDesired := map[bool]string{true: "stopped", false: ""}[tc.sub == "survival"]; desired != wantDesired {
				t.Errorf("published desired_state = %q, want %q", desired, wantDesired)
			}
		})
	}
}
