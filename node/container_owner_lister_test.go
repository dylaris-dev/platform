package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

// R55-06: on a BYON host the node drives the customer's own docker.sock, and
// the listers took every mc_ name with no owner label as this node's. The
// startup ReconcileRedisEnv then stopped and removed a customer's own
// mc_survival because it lacked our Redis env. An unlabelled container is
// listed only when this node's storage holds that server's directory (or the
// node tombstoned it); a labelled one keeps the owner-label rule.
func TestListersSkipUnlabelledContainersThatAreNotOurs(t *testing.T) {
	prev := nodeSecretDir
	nodeSecretDir = t.TempDir()
	t.Cleanup(func() { nodeSecretDir = prev })
	t.Setenv("NODE_ID", "node-a")

	const (
		legacy   = "11111111-2222-3333-4444-555555555555" // unlabelled, dir here
		otherOne = "22222222-2222-3333-4444-555555555555" // node-b's, dir here too
		labelled = "33333333-2222-3333-4444-555555555555" // ours by label, no dir
		deleted  = "44444444-2222-3333-4444-555555555555" // unlabelled, tombstoned, dir gone
	)
	root := t.TempDir()
	for _, u := range []string{legacy, otherOne} {
		if err := os.Mkdir(filepath.Join(root, u), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(tombstonePath(deleted), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	list := `[
	 {"Id":"a","Names":["/mc_survival"],"Labels":{},"State":"running"},
	 {"Id":"b","Names":["/mc_server_1"],"Labels":{},"State":"running"},
	 {"Id":"c","Names":["/mc_` + legacy + `"],"Labels":{},"State":"running"},
	 {"Id":"d","Names":["/mc_` + otherOne + `"],"Labels":{"` + ownerLabel + `":"node-b"},"State":"running"},
	 {"Id":"e","Names":["/mc_` + labelled + `"],"Labels":{"` + ownerLabel + `":"node-a"},"State":"running"},
	 {"Id":"f","Names":["/mc_` + deleted + `"],"Labels":{},"State":"running"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			w.Write([]byte(list))
			return
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	dm := &DockerManager{cli: cli, ctx: t.Context(), storageMgr: &StorageManager{paths: []string{root}}}

	want := []string{legacy, labelled, deleted}

	running, err := dm.ListRunningMCContainers()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range running {
		got = append(got, c.UUID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("ListRunningMCContainers = %v, want %v", got, want)
	}

	all, err := dm.ListAllMCContainers()
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	for _, c := range all {
		got = append(got, c.UUID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("ListAllMCContainers = %v, want %v", got, want)
	}
}
