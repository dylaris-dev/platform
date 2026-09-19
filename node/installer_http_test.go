package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The failure these clients exist for: a peer that accepts the connection and
// then never responds. With http.DefaultClient that hangs forever and the server
// stays in `installing` with no outcome either way.
func TestInstallerMetaClient_TimesOutOnASilentServer(t *testing.T) {
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-stall // accept, then say nothing
	}))
	// Order matters and is LIFO: srv.Close() blocks until its handlers return,
	// so the channel has to be closed FIRST or the test deadlocks on its own
	// cleanup. Declaring the close last is what makes it run first.
	defer srv.Close()
	defer close(stall)

	// A local copy so the test does not wait out the production 30s.
	client := &http.Client{Timeout: 150 * time.Millisecond}

	done := make(chan error, 1)
	go func() {
		resp, err := client.Get(srv.URL)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a silent server returned no error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the request hung despite a client timeout")
	}
}

// The metadata client must have an overall deadline; that is its whole point.
func TestInstallerMetaClient_HasATimeout(t *testing.T) {
	if installerMetaClient.Timeout <= 0 {
		t.Fatal("installerMetaClient has no timeout - http.DefaultClient's behaviour is what this replaced")
	}
	if installerMetaClient.Timeout != installerMetaTimeout {
		t.Errorf("timeout = %v, want %v", installerMetaClient.Timeout, installerMetaTimeout)
	}
}

// The download client must NOT have an overall deadline: a server JAR on a thin
// link legitimately takes minutes, and a blanket timeout would abort a transfer
// that is making progress.
func TestInstallerDownloadClient_HasNoOverallDeadline(t *testing.T) {
	if installerDownloadClient.Timeout != 0 {
		t.Fatalf("installerDownloadClient.Timeout = %v, want 0 - it would cut off slow but working downloads",
			installerDownloadClient.Timeout)
	}
}

// It must still bound the phases that can hang without transferring anything,
// or it would be no better than http.DefaultClient for a stalled peer.
func TestInstallerDownloadClient_BoundsTheStallablePhases(t *testing.T) {
	tr, ok := installerDownloadClient.Transport.(uaTransport).base.(*http.Transport)
	if !ok {
		t.Fatal("installerDownloadClient has no *http.Transport, so nothing bounds a stalled connection")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("TLSHandshakeTimeout unset")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout unset - a peer that accepts and never answers would hang")
	}
	if tr.DialContext == nil {
		t.Error("DialContext unset, so connecting is unbounded")
	}
}

// A custom Transport starts from the ZERO value, not from DefaultTransport, so
// a nil Proxy means HTTP_PROXY/HTTPS_PROXY are ignored - every download on a
// node behind a proxy would fail, and only there, which is the worst kind of
// regression to find. The meta client keeps DefaultTransport and needs no check.
//
// Asserting the hook is wired is the whole test: resolving an actual proxy URL
// would be testing net/http's env parsing, which caches the environment in a
// sync.Once and so cannot be driven from a test anyway.
func TestInstallerDownloadClient_HonoursProxyEnvironment(t *testing.T) {
	tr, ok := installerDownloadClient.Transport.(uaTransport).base.(*http.Transport)
	if !ok {
		t.Fatal("installerDownloadClient has no *http.Transport")
	}
	if tr.Proxy == nil {
		t.Fatal("Transport.Proxy is nil - HTTP_PROXY/HTTPS_PROXY would be ignored")
	}
}

// A response-header stall is the download client's specific hazard: bytes never
// start flowing, so no progress-based rule would ever fire.
func TestInstallerDownloadClient_ResponseHeaderStall(t *testing.T) {
	stall := make(chan struct{})
	defer close(stall)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-stall // connection accepted, no response line ever sent
	}()

	client := &http.Client{Transport: &http.Transport{
		ResponseHeaderTimeout: 150 * time.Millisecond,
	}}

	done := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://" + ln.Addr().String())
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a header-stalling server returned no error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the request hung despite ResponseHeaderTimeout")
	}
}

// A peer that sends headers and then goes quiet MID-BODY is the gap the
// Transport cannot close: ResponseHeaderTimeout stops applying the moment the
// headers arrive, and a blanket Client.Timeout would kill a slow-but-working
// transfer. Without the watchdog this test does not fail, it HANGS - the server
// never closes and io.Copy waits forever, which is exactly what left a server
// stuck in `installing` with no way out but a manual reset.
func TestDownloadFile_AbandonsAStalledBody(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("the first bytes arrive fine"))
		w.(http.Flusher).Flush()
		<-release // and then nothing, ever
	}))
	// LIFO: the handler has to be let go BEFORE Close waits on it, or Close
	// blocks on the very stall this test creates.
	defer srv.Close()
	defer close(release)

	dest := filepath.Join(t.TempDir(), "server.jar")
	start := time.Now()
	err := downloadFileWithin(srv.URL, dest, 150*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a stalled download returned no error")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("error = %v, want it to name the stall so the log says why", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v - the watchdog did not fire", elapsed)
	}
}

// The window bounds SILENCE, not total duration: a transfer that keeps
// delivering bytes must run to completion however long it takes, or a thin link
// would lose every large JAR.
//
// Asserted on stallGuard directly rather than by timing a real download. The
// end-to-end version slept through 30 chunks and relied on the total exceeding
// the stall window while every gap stayed far under it; it failed twice on a
// loaded CI runner, first at a 3.3x margin and then at 15x, both times on
// commits that could not have touched it. Widening the margin is a race against
// whatever else the machine happens to be doing. What the guard actually
// promises owes nothing to the clock: a read that produced bytes pushes the
// deadline back, and a read that produced none does not.
type resetRecorder struct{ windows []time.Duration }

func (r *resetRecorder) Reset(d time.Duration) bool {
	r.windows = append(r.windows, d)
	return true
}

func TestStallGuard_ProgressPushesTheDeadlineBack(t *testing.T) {
	const window = 300 * time.Millisecond
	rec := &resetRecorder{}
	sg := &stallGuard{r: strings.NewReader("chunkchunkchunk"), timer: rec, window: window}

	buf := make([]byte, 5)
	for i := range 3 {
		if n, err := sg.Read(buf); err != nil || n == 0 {
			t.Fatalf("read %d: n=%d err=%v", i, n, err)
		}
	}
	if len(rec.windows) != 3 {
		t.Fatalf("deadline pushed back %d times for 3 reads carrying bytes, want 3", len(rec.windows))
	}
	for i, w := range rec.windows {
		if w != window {
			t.Errorf("reset %d used %v, want the full window %v", i, w, window)
		}
	}
}

// A read that produced nothing must NOT push the deadline back, or a peer that
// holds the connection open while sending zero bytes would never be timed out -
// which is the case the watchdog exists for.
func TestStallGuard_NoBytesDoesNotPushTheDeadlineBack(t *testing.T) {
	rec := &resetRecorder{}
	sg := &stallGuard{r: strings.NewReader(""), timer: rec, window: time.Second}

	if n, err := sg.Read(make([]byte, 4)); n != 0 || err == nil {
		t.Fatalf("read = (%d, %v), want (0, EOF) from an empty reader", n, err)
	}
	if len(rec.windows) != 0 {
		t.Fatalf("deadline pushed back %d times on a read that carried no bytes", len(rec.windows))
	}
}
