package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A backend that takes the connection and stops reading held a command slot
// for partPutTimeout (30 minutes) per attempt: the response-header timeout
// starts only after the body is written. The stall timer ends it.
func TestAPartPutNobodyReadsIsCutOff(t *testing.T) {
	prev := partStallTimeout
	t.Cleanup(func() { partStallTimeout = prev })
	partStallTimeout = 300 * time.Millisecond

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never reads the body
	}))
	defer srv.Close()
	defer close(release)

	u := &partUploader{client: srv.Client()}
	// Far beyond what the socket buffers on both ends hold, so the writer blocks.
	data := make([]byte, 64<<20)
	done := make(chan error, 1)
	go func() { done <- u.put(context.Background(), srv.URL, data) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("put: %v, want the stall to cancel it", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a PUT nobody read was not cut off")
	}
}

// The body is no longer a *bytes.Reader, so the length is set by hand: a
// presigned PUT must carry it (and not go out chunked), on a redirect too.
func TestAPartPutCarriesItsLength(t *testing.T) {
	data := []byte("part-bytes")
	var gotLen int64
	var got []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/first", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/second", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/second", func(w http.ResponseWriter, r *http.Request) {
		gotLen = r.ContentLength
		got, _ = io.ReadAll(r.Body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u := &partUploader{client: srv.Client()}
	if err := u.put(context.Background(), srv.URL+"/first", data); err != nil {
		t.Fatalf("put: %v", err)
	}
	if gotLen != int64(len(data)) || string(got) != string(data) {
		t.Fatalf("server saw length %d body %q", gotLen, got)
	}
}
