package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"
)

// Q15: a failed Docker lookup of the Link or the node is not "no Link". Read
// as one, the round applied an allowlist without the Link to every server and
// refused every new player until a later round found it again.
func TestAFailedInfraLookupIsAnErrorNotAShorterSet(t *testing.T) {
	const self = "selfnode"
	for _, tc := range []struct {
		name       string
		listStatus int
		selfStatus int
		want       string // joined addresses; "" with wantErr
		wantErr    bool
	}{
		{"both found", 200, 200, "10.0.0.2,10.0.0.9", false},
		{"Link list fails", 500, 200, "", true},
		{"node inspect fails", 200, 500, "", true},
		{"node ref never resolves", 200, 404, "10.0.0.9", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/containers/json"):
					if tc.listStatus != 200 {
						w.WriteHeader(tc.listStatus)
						w.Write([]byte(`{"message":"daemon busy"}`))
						return
					}
					json.NewEncoder(w).Encode([]map[string]any{{
						"Names": []string{"/dylaris_link"}, "Image": "ghcr.io/x/gateway-link:1",
						"NetworkSettings": map[string]any{"Networks": map[string]any{"net": map[string]any{"IPAddress": "10.0.0.9"}}},
					}})
				case strings.HasSuffix(r.URL.Path, "/containers/"+self+"/json"):
					if tc.selfStatus != 200 {
						w.WriteHeader(tc.selfStatus)
						w.Write([]byte(`{"message":"nope"}`))
						return
					}
					json.NewEncoder(w).Encode(map[string]any{
						"Name":            "/" + self,
						"NetworkSettings": map[string]any{"Networks": map[string]any{"net": map[string]any{"IPAddress": "10.0.0.2"}}},
					})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cli.Close() })
			dm := &DockerManager{cli: cli, ctx: t.Context(), selfContainer: self}

			got, err := dm.infraPeers()
			if (err != nil) != tc.wantErr {
				t.Fatalf("infraPeers err = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && linkAddrsKey(got) != tc.want {
				t.Fatalf("infraPeers = %v, want %s", got, tc.want)
			}
		})
	}
}

// Q16: a crash restart through PowerAction("start") reuses the container, so
// its id does not change. A sample that failed while it was down is what says
// the start time must be read again, or readiness stays "ready" from the old
// run and a server still loading its world is reported online.
func TestASampleGapReinspectsTheContainer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		gap          bool
		lastID, id   string
		wantReinspec bool
	}{
		{"steady", false, "a", "a", false},
		{"first sample", false, "", "a", false},
		{"recreated", false, "a", "b", true},
		{"restarted in place", true, "a", "a", true},
		{"gap on the first sample", true, "", "a", true},
		{"no id", true, "a", "", false},
	} {
		if got := reinspectAfterSample(tc.gap, tc.lastID, tc.id); got != tc.wantReinspec {
			t.Errorf("%s: reinspectAfterSample(%v, %q, %q) = %v, want %v", tc.name, tc.gap, tc.lastID, tc.id, got, tc.wantReinspec)
		}
	}
}

// Q17: the scan cancels the collector as soon as the container is gone. A
// trip running on that cancelled context lost the marker and still wrote
// disk_full, and only the marker lets the stopped-server sweep lift the hold:
// the server stayed disk_full, start refused, for good.
func TestADiskTripOnACancelledCollectorKeepsItsMarker(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	const uuid = "aaaaaaaa-1111-2222-3333-444444444444"
	t.Cleanup(func() { diskGuardTrips.Delete(uuid) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	holdForDiskFull(ctx, rdb, uuid, "live", &DiskUsagePayload{Total: 2, Limit: 1}, func() error { return nil })

	status, _ := mr.Get("dylaris:server:" + uuid + ":status")
	if status == "disk_full" && !mr.Exists(diskFullKey(uuid)) {
		t.Fatal("disk_full was written without the marker that is the only way to release it")
	}
	if !mr.Exists(diskFullKey(uuid)) {
		t.Fatal("the trip's marker was lost to the cancelled collector context")
	}
}

// Q18: a server a transfer is landing into is not hosted here yet. Its partial
// copy is under any limit, and the sweep read it as "resolved" and lifted the
// uuid-global disk_full hold of the node that does host it.
func TestATransferLandingHereIsNotALocalServer(t *testing.T) {
	prevMgr := globalStorageMgr
	t.Cleanup(func() { globalStorageMgr = prevMgr })
	root := t.TempDir()
	globalStorageMgr = NewStorageManager(root, nil)
	const hosted, landing = "aaaaaaaa-1111-2222-3333-444444444444", "bbbbbbbb-1111-2222-3333-444444444444"
	for _, d := range []string{hosted, landing} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	migrateInFlight.Store(landing, struct{}{})
	t.Cleanup(func() { migrateInFlight.Delete(landing) })

	got := localServerUUIDs()
	if len(got) != 1 || got[0] != hosted {
		t.Fatalf("local servers = %v, want only %s", got, hosted)
	}
}
