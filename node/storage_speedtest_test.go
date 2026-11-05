package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	pb "dylaris-proto/node"
)

func TestStorageSpeedTest(t *testing.T) {
	var got atomic.Int64
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got.Store(n)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	prev := objectTransferClient
	objectTransferClient = srv.Client()
	t.Cleanup(func() { objectTransferClient = prev })
	h := &StreamHandler{}

	resp := h.handleStorageSpeedTest("r1", &pb.StorageSpeedTestReq{Url: srv.URL, SizeBytes: 1 << 20})
	r := resp.GetStorageSpeedTestResp()
	if r == nil {
		t.Fatalf("got %v, want a result", resp)
	}
	if r.Bytes != 1<<20 || got.Load() != 1<<20 || !r.Lan {
		t.Fatalf("result %+v, server got %d bytes; want 1 MiB both, lan (loopback)", r, got.Load())
	}

	for name, req := range map[string]*pb.StorageSpeedTestReq{
		"zero size": {Url: srv.URL},
		"too large": {Url: srv.URL, SizeBytes: maxSpeedTestBytes + 1},
	} {
		if h.handleStorageSpeedTest("r", req).GetError() == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	speedTestRunning.Store(true)
	if e := h.handleStorageSpeedTest("r", &pb.StorageSpeedTestReq{Url: srv.URL, SizeBytes: 10}).GetError(); e == nil || e.Code != 409 {
		t.Errorf("a second concurrent test: %v, want 409", e)
	}
	speedTestRunning.Store(false)

	status = http.StatusForbidden
	if e := h.handleStorageSpeedTest("r", &pb.StorageSpeedTestReq{Url: srv.URL, SizeBytes: 10}).GetError(); e == nil {
		t.Error("a refused PUT reported a speed")
	}
}

// H41: the body was drained before the status check, so the S3 error code the
// other storage paths report never reached the operator.
func TestStorageSpeedTestReportsTheS3ErrorCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><Error><Code>SignatureDoesNotMatch</Code></Error>`)
	}))
	defer srv.Close()
	_, _, _, err := runSpeedTest(context.Background(), srv.Client(), srv.URL, 10)
	if err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Fatalf("err = %v, want the S3 error code", err)
	}
}

// A tenant's bucket goes through the guarded client, which refuses a private
// address unless the operator allowed it.
func TestStorageSpeedTestGuardedRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	e := (&StreamHandler{}).handleStorageSpeedTest("r", &pb.StorageSpeedTestReq{Url: srv.URL, SizeBytes: 10, Guarded: true}).GetError()
	if e == nil {
		t.Fatal("a guarded speed test reached a loopback address")
	}
}
