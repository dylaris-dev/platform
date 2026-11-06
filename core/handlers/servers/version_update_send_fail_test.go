package servers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// sendFailStore records the server columns as a move leaves them.
type sendFailStore struct {
	moveStore
	setup  []string
	status string
}

func (f *sendFailStore) UpdateServerSetup(_ int, image, command, sub, flags, loader, mc, build string) error {
	f.setup = []string{image, command, sub, flags, loader, mc, build}
	return nil
}
func (f *sendFailStore) UpdateServerStatus(_ int, s string) error { f.status = s; return nil }

// dropStore is a move whose unavailable mods are deleted.
type dropStore struct{ moveStore }

func (f *dropStore) DeleteServerMod(int, int) error { return nil }

// A move whose dispatch never reached the queue left the DB on the target
// version and the server "installing" for good: no node got the command, so
// none would ever report a failure to undo it.
func TestVersionUpdateSendFailurePutsTheServerBack(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	dead := miniredis.RunT(t)
	queueRdb := redis.NewClient(&redis.Options{Addr: dead.Addr()})
	dead.Close()

	fs := &sendFailStore{}
	fs.srv = models.Server{ID: 1, UUID: "u-1", NodeID: 3, ActiveSubServer: "main", Status: "stopped",
		InstallerType: "fabric", MinecraftVersion: "1.20.1", BuildNumber: "0.16.9", GameImage: "img-java17",
		StartCommand: "java -jar x.jar", ExtraJvmFlags: "-Dx=1"}
	h := NewServerModsHandler(testDeps(Deps{Store: fs, Redis: rdb, Queue: services.NewQueueService(queueRdb)}))
	req := httptest.NewRequest(http.MethodPost, "/api/servers/1/version-update",
		strings.NewReader(`{"minecraft":"1.21.1","installerVersion":"0.16.10","javaImage":"`+platformImagePrefix+`21"}`))
	req = mux.SetURLVars(req, map[string]string{"id": "1"})
	rec := httptest.NewRecorder()
	h.VersionUpdate(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("answered %d %s, want 500", rec.Code, rec.Body.String())
	}
	want := []string{"img-java17", "java -jar x.jar", "main", "-Dx=1", "fabric", "1.20.1", "0.16.9"}
	if strings.Join(fs.setup, "|") != strings.Join(want, "|") {
		t.Errorf("server columns left as %q, want %q", fs.setup, want)
	}
	if fs.status != "stopped" {
		t.Errorf("status left %q, want stopped", fs.status)
	}
	if n, _ := rdb.Exists(context.Background(), services.VersionUpdateUndoKey("u-1")).Result(); n != 0 {
		t.Error("an undo record was left for a move that never reached the node")
	}
}

// Each old jar is removed from the directory its row records. The move used to
// send names only, all taken from the loader's directory, so a mod recorded in
// plugins/ on a server now declared Fabric stayed behind with no row left.
// "remove" stays for a node that predates "removeFrom".
func TestVersionUpdateNamesEachRemovalsDirectory(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	fs := &dropStore{}
	fs.srv = models.Server{ID: 1, UUID: "u-1", NodeID: 3, ActiveSubServer: "main", Status: "stopped",
		InstallerType: "fabric", MinecraftVersion: "1.20.1"}
	fs.mods = []models.ServerMod{
		{ID: 2, ServerID: 1, Title: "A", FileName: "a.jar", TargetDir: "mods"},
		{ID: 3, ServerID: 1, Title: "P", FileName: "p.jar", TargetDir: "plugins"},
		{ID: 4, ServerID: 1, Title: "U", FileName: "u.jar"},
	}
	h := NewServerModsHandler(testDeps(Deps{Store: fs, Redis: rdb, Queue: services.NewQueueService(rdb)}))
	req := httptest.NewRequest(http.MethodPost, "/api/servers/1/version-update",
		strings.NewReader(`{"minecraft":"1.21.1","dropUnavailable":true,"javaImage":"`+platformImagePrefix+`21"}`))
	rec := httptest.NewRecorder()
	h.VersionUpdate(rec, mux.SetURLVars(req, map[string]string{"id": "1"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d %s", rec.Code, rec.Body.String())
	}
	msgs, err := rdb.XRange(context.Background(), "dylaris:node:tok:cmds", "-", "+").Result()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("queued %d commands (%v)", len(msgs), err)
	}
	var cmd struct {
		Config struct {
			Remove     []string `json:"remove"`
			RemoveFrom []struct {
				Dir      string `json:"dir"`
				FileName string `json:"fileName"`
			} `json:"removeFrom"`
		} `json:"config"`
	}
	for _, v := range msgs[0].Values {
		if s, ok := v.(string); ok {
			json.Unmarshal([]byte(s), &cmd)
		}
	}
	if strings.Join(cmd.Config.Remove, ",") != "a.jar,p.jar,u.jar" {
		t.Errorf("remove = %v", cmd.Config.Remove)
	}
	var got []string
	for _, r := range cmd.Config.RemoveFrom {
		got = append(got, r.Dir+"/"+r.FileName)
	}
	if strings.Join(got, ",") != "mods/a.jar,plugins/p.jar,mods/u.jar" {
		t.Errorf("removeFrom = %v", got)
	}
}
