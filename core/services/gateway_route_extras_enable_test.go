package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/services/remotegw"
)

// Review M-4 (1): the caller goes away while the provider is switching voice
// on. The call runs on a context detached from the caller's, so the provider's
// yes is still recorded here: the two sides never end up "provider on, local
// off" because a browser tab was closed.
func TestEnableServerVoiceOutlivesTheCaller(t *testing.T) {
	e := newExtrasEnv(t)
	srv, d := e.addServer(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Forwards to the provider, then drops the caller before answering.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, _ := http.NewRequestWithContext(context.Background(), r.Method, e.fake.URL()+r.URL.Path, bytes.NewReader(body))
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		if strings.HasSuffix(r.URL.Path, "/voice") {
			cancel()
			time.Sleep(50 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	g := NewRemoteGateway(e.st, func() *remotegw.Client { return remotegw.New(proxy.URL, e.fake.Key) })
	port, err := g.EnableServerVoice(ctx, srv.UUID, d, "plasmo")
	if err != nil {
		t.Fatalf("enable with the caller gone: %v", err)
	}
	if r := e.row(d); r.VoicePort == nil || *r.VoicePort != port {
		t.Fatalf("provider on, row %+v", r)
	}
}

// Review M-4 (2): after a 502 the provider's list decides whether Bedrock was
// stored, and only a listing with the Java edition that was asked counts. A
// Bedrock the provider holds with the other Java setting is not this request
// stored: the reservation goes back.
func TestAddBedrockUntoldNeedsTheAskedJavaEdition(t *testing.T) {
	e := newExtrasEnv(t)
	srv, d := e.addServer(1)
	if _, err := e.c.SetBedrock(context.Background(), d, false, true); err != nil { // Bedrock only, held there
		t.Fatal(err)
	}
	g := NewRemoteGateway(e.st, func() *remotegw.Client { return e.c })
	e.fake.FailNext(1, http.StatusBadGateway, "internal", 0) // the PUT, not stored
	if _, err := g.AddBedrock(context.Background(), srv.ID, srv.UUID, d, true); err == nil {
		t.Fatal("a 502 the list contradicts was taken as stored")
	}
	if r := e.row(d); r.Edition != "" {
		t.Fatalf("edition %q, want the reservation taken back", r.Edition)
	}
	if _, ok := e.st.keys[srv.ID]; ok {
		t.Fatal("a key was stored")
	}
}

// Review M-4 (3): the key download worked but storing it failed: the
// reservation goes back, nothing is recorded as on.
func TestAddBedrockUndoesWhenTheKeyCannotBeStored(t *testing.T) {
	e := newExtrasEnv(t)
	srv, d := e.addServer(1)
	e.st.keySetErr = errors.New("connection reset")
	g := NewRemoteGateway(e.st, func() *remotegw.Client { return e.c })
	if _, err := g.AddBedrock(context.Background(), srv.ID, srv.UUID, d, true); err == nil {
		t.Fatal("add succeeded")
	}
	if r := e.row(d); r.Edition != "" {
		t.Fatalf("edition %q, want the reservation taken back", r.Edition)
	}
}
