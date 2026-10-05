package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

// dockerWithWorkingDir answers the container inspect with the given working
// directory, or 404 when it is "", or 500 when it is "!".
func dockerWithWorkingDir(t *testing.T, wd string) *DockerManager {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch wd {
		case "":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"No such container"}`))
		case "!":
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"daemon busy"}`))
		default:
			w.Write([]byte(`{"Id":"x","State":{"Running":true},"Config":{"WorkingDir":"` + wd + `"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return &DockerManager{cli: cli, ctx: t.Context()}
}

// Deleting a sub-server killed the running server whichever one it was: an
// inactive one is not held by the container and needs no teardown.
func TestOnlyTheRunningSubServerHoldsTheContainer(t *testing.T) {
	const uuid = "15151515-2222-3333-4444-555555555555"
	for _, c := range []struct {
		wd, sub string
		want    bool
	}{
		{"/data/survival", "survival", true},
		{"/data/survival", "creative", false},
		{"/data/survival", "surviv", false},
		{"", "survival", false}, // no container
		{"!", "survival", true}, // could not check: treat as held
	} {
		if got := dockerWithWorkingDir(t, c.wd).containerUsesSubServer(uuid, c.sub); got != c.want {
			t.Errorf("container in %q, deleting %q: held = %v, want %v", c.wd, c.sub, got, c.want)
		}
	}
}

// The teardown runs before the rename only for the sub-server in use, and
// again only if the rename of an inactive one fails.
func TestDeleteSubServerTearsDownOnlyWhenNeeded(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i, j := strings.Index(s, "\tcase \"delete_sub_server\":"), strings.Index(s, "// Drop the per-sub-server log stream")
	k := strings.Index(s, "renameErr := os.Rename(subServerPath, pendingPath)")
	if i < 0 || j < i || k < j {
		t.Fatal("the delete_sub_server case moved; move this assertion with it")
	}
	head := s[i:j]
	if !strings.Contains(head, "if inUse {\n\t\t\tteardown()\n\t\t}") {
		t.Error("the container is torn down before the rename without asking whether it uses the sub-server")
	}
	if strings.Count(head, "teardown()") != 1 {
		t.Error("an unconditional teardown is back before the rename")
	}
	tail := s[k : k+400]
	f := strings.Index(tail, "if renameErr != nil && !inUse {")
	if f < 0 || !strings.Contains(tail[f:], "teardown()") {
		t.Error("a failed rename of an inactive sub-server no longer falls back to the teardown")
	}
}

// A server left running by an inactive delete must not be reported stopped:
// the edge turns new logins away for a "stopped" server.
func TestDeleteSubServerReportsStoppedOnlyAfterATeardown(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "\tcase \"delete_sub_server\":")
	j := strings.Index(s[i:], "\tcase \"reinstall\":")
	if i < 0 || j < 0 {
		t.Fatal("the delete_sub_server case moved; move this assertion with it")
	}
	body := s[i : i+j]
	if !strings.Contains(body, "} else if toreDown {\n\t\t\trdb.Set(ctx, statusKey, \"stopped\"") {
		t.Error("the delete reports stopped without having stopped anything")
	}
	if !strings.Contains(body, "teardown := func() {\n\t\t\ttoreDown = true") {
		t.Error("the teardown no longer records that it ran")
	}
}
