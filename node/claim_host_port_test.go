package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"
)

// A config change for a server with no container (stopped after a move, a
// sub-server delete, a storage move) only reached the saved config. The port
// ledger kept its old answer, so a moved server's kept port could be handed to
// a new server, and an admin-set port was rebound back to the old one at the
// next start.
func TestAHostPortChangeWithoutAContainerIsClaimed(t *testing.T) {
	const uuid = "cdcdcdcd-2222-3333-4444-555555555555"
	const other = "efefefef-2222-3333-4444-555555555555"
	r, f, p, cp, io, pids := getModes()
	t.Cleanup(func() { setModes(r, f, p, cp, io, pids) })
	setModes("ip_port", f, p, 25565, io, pids)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"No such container"}`))
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })

	for _, tc := range []struct {
		name    string
		apply   func(*DockerManager, ServerConfig) error
		held    int // the server's ledger port before the change, 0 for none
		taken   bool
		deleted bool // a delete of the server already ran on this node
		wantErr bool
		want    int
	}{
		{"reconfigure after a move", func(dm *DockerManager, c ServerConfig) error { return dm.RecreateKeepingRunState(c) }, 0, false, false, false, 25610},
		{"update_resources replaces the held port", func(dm *DockerManager, c ServerConfig) error { _, err := dm.UpdateResources(c); return err }, 25601, false, false, false, 25610},
		{"reconfigure onto a port another server holds", func(dm *DockerManager, c ServerConfig) error { return dm.RecreateKeepingRunState(c) }, 0, true, false, true, 0},
		{"update_resources onto a port another server holds", func(dm *DockerManager, c ServerConfig) error { _, err := dm.UpdateResources(c); return err }, 25601, true, false, true, 25601},
		// Nothing frees a ledger entry of a deleted server until a restart.
		{"reconfigure of a deleted server", func(dm *DockerManager, c ServerConfig) error { return dm.RecreateKeepingRunState(c) }, 0, false, true, false, 0},
		{"update_resources of a deleted server", func(dm *DockerManager, c ServerConfig) error { _, err := dm.UpdateResources(c); return err }, 0, false, true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			dm := &DockerManager{cli: cli, ctx: t.Context(), localDataPath: t.TempDir()}
			dm.portMgr = NewPortManager(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "node-a", 25600, 25699, seqMode)
			if tc.held > 0 {
				if err := dm.portMgr.SetPort(uuid, tc.held); err != nil {
					t.Fatal(err)
				}
			}
			if tc.taken {
				if err := dm.portMgr.SetPort(other, 25610); err != nil {
					t.Fatal(err)
				}
			}
			if tc.deleted {
				markServerDeleted(uuid)
				t.Cleanup(func() { forgetTombstoneInMemory(uuid) })
			}
			cfg := ServerConfig{UUID: uuid}
			cfg.Docker.HostPort = 25610
			err := tc.apply(dm, cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, errHostPortClaim) {
				t.Fatalf("err = %v, want errHostPortClaim", err)
			}
			if got := dm.portMgr.GetPort(uuid); got != tc.want {
				t.Fatalf("ledger holds %d for the server, want %d", got, tc.want)
			}
			if tc.taken && dm.portMgr.GetPort(other) != 25610 {
				t.Fatalf("the other server lost its port")
			}
		})
	}
}
