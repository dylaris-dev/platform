package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/storage/modpack"
)

// serveFakeProvider is a ModpackStorageProvider that FAILS THE TEST if Get is
// called.
//
// That is the point of it. Get returns the whole object as a []byte, and
// serving a pack that way put the entire thing in Core's heap once per
// concurrent request - the defect these handlers were changed to remove. A
// regression would not show up as a wrong response, only as memory that grows
// with traffic, so the guard has to be "the buffering call was never made"
// rather than anything about the bytes.
type serveFakeProvider struct {
	t *testing.T

	body      string
	size      int64
	streamErr error

	presignURL string
	presignErr error

	streamCalls  int
	presignCalls int
}

func (f *serveFakeProvider) Get(context.Context, string) ([]byte, error) {
	f.t.Helper()
	f.t.Fatal("Get was called: the handler is buffering the whole object again instead of streaming it")
	return nil, nil
}

func (f *serveFakeProvider) Stream(context.Context, string) (io.ReadCloser, int64, error) {
	f.streamCalls++
	if f.streamErr != nil {
		return nil, 0, f.streamErr
	}
	return io.NopCloser(strings.NewReader(f.body)), f.size, nil
}

func (f *serveFakeProvider) DownloadURL(context.Context, string, time.Duration) (string, error) {
	f.presignCalls++
	return f.presignURL, f.presignErr
}

func (f *serveFakeProvider) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func (f *serveFakeProvider) Put(context.Context, string, []byte) error { return nil }
func (f *serveFakeProvider) PutStream(context.Context, string, io.Reader, int64) error {
	return nil
}
func (f *serveFakeProvider) Delete(context.Context, string) error { return nil }
func (f *serveFakeProvider) Stat(context.Context, string) (int64, bool, error) {
	return f.size, true, nil
}

func serveRequest(t *testing.T, prov modpack.ModpackStorageProvider, mode modpackDelivery) (*httptest.ResponseRecorder, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/solder/mirror/modpacks/pack.mrpack", nil)
	err := serveModpackObject(rec, req, prov, "modpacks/pack.mrpack", mode, "application/zip", "pack.mrpack")
	return rec, err
}

// TestServeModpackObject_StreamsInsteadOfBuffering is the regression guard. The
// fake fails the test from inside Get, so this passes only while the handler
// takes the streaming path.
func TestServeModpackObject_StreamsInsteadOfBuffering(t *testing.T) {
	for _, mode := range []struct {
		name string
		mode modpackDelivery
	}{
		{name: "stream", mode: deliverStream},
		{name: "redirect falling back to stream", mode: deliverRedirect},
	} {
		t.Run(mode.name, func(t *testing.T) {
			// No presign URL, so deliverRedirect has to fall through.
			f := &serveFakeProvider{t: t, body: "pack-bytes", size: 10}

			rec, err := serveRequest(t, f, mode.mode)
			if err != nil {
				t.Fatalf("serveModpackObject: %v", err)
			}
			if f.streamCalls != 1 {
				t.Errorf("Stream calls = %d, want 1", f.streamCalls)
			}
			if rec.Body.String() != "pack-bytes" {
				t.Errorf("body = %q, want %q", rec.Body.String(), "pack-bytes")
			}
			if got := rec.Header().Get("Content-Length"); got != "10" {
				t.Errorf("Content-Length = %q, want %q", got, "10")
			}
			if got := rec.Header().Get("Content-Type"); got != "application/zip" {
				t.Errorf("Content-Type = %q, want %q", got, "application/zip")
			}
		})
	}
}

// TestServeModpackObject_StreamModeNeverRedirects pins the Solder mirror's
// deliberate limit. That route serves the Technic launcher, whose redirect
// behaviour this codebase cannot verify, so deliverStream must not quietly
// start handing out 302s just because the backend gained the ability to
// presign.
func TestServeModpackObject_StreamModeNeverRedirects(t *testing.T) {
	f := &serveFakeProvider{t: t, body: "pack-bytes", size: 10, presignURL: "https://objects.example/pack"}

	rec, err := serveRequest(t, f, deliverStream)
	if err != nil {
		t.Fatalf("serveModpackObject: %v", err)
	}
	if f.presignCalls != 0 {
		t.Errorf("DownloadURL calls = %d, want 0: stream mode must not ask for a URL at all", f.presignCalls)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "pack-bytes" {
		t.Errorf("body = %q, want the object streamed", rec.Body.String())
	}
}

// TestServeModpackObject_RedirectsWhenTheBackendCan covers the case worth the
// most: on a presigning backend the bytes never enter this process.
func TestServeModpackObject_RedirectsWhenTheBackendCan(t *testing.T) {
	f := &serveFakeProvider{t: t, body: "pack-bytes", size: 10, presignURL: "https://objects.example/pack?sig=x"}

	rec, err := serveRequest(t, f, deliverRedirect)
	if err != nil {
		t.Fatalf("serveModpackObject: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != f.presignURL {
		t.Errorf("Location = %q, want %q", got, f.presignURL)
	}
	if f.streamCalls != 0 {
		t.Errorf("Stream calls = %d, want 0: a redirect must not also read the object", f.streamCalls)
	}
}

// TestServeModpackObject_StreamsWhenPresigningFails: a backend that should be
// able to presign but errors is a misconfiguration, not a reason to fail a
// download the process can serve perfectly well.
func TestServeModpackObject_StreamsWhenPresigningFails(t *testing.T) {
	f := &serveFakeProvider{t: t, body: "pack-bytes", size: 10, presignErr: errors.New("credentials expired")}

	rec, err := serveRequest(t, f, deliverRedirect)
	if err != nil {
		t.Fatalf("serveModpackObject: %v", err)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "pack-bytes" {
		t.Fatalf("status = %d body = %q, want the object streamed as a fallback", rec.Code, rec.Body.String())
	}
}

// TestServeModpackObject_OmitsContentLengthWhenTheSizeIsUnknown: announcing a
// length the body will not match is worse than announcing none, and measuring
// the stream to find out would mean buffering it - exactly what this avoids.
func TestServeModpackObject_OmitsContentLengthWhenTheSizeIsUnknown(t *testing.T) {
	f := &serveFakeProvider{t: t, body: "pack-bytes", size: modpack.SizeUnknown}

	rec, err := serveRequest(t, f, deliverStream)
	if err != nil {
		t.Fatalf("serveModpackObject: %v", err)
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want it absent for an unknown size", got)
	}
	if rec.Body.String() != "pack-bytes" {
		t.Errorf("body = %q, want the object still served", rec.Body.String())
	}
}

// TestServeModpackObject_ReportsAMissingObject: the caller turns this into the
// 404 its own API shape wants, so the sentinel has to survive.
func TestServeModpackObject_ReportsAMissingObject(t *testing.T) {
	f := &serveFakeProvider{t: t, streamErr: modpack.ErrNotFound}

	rec, err := serveRequest(t, f, deliverStream)
	if !errors.Is(err, modpack.ErrNotFound) {
		t.Fatalf("error = %v, want modpack.ErrNotFound", err)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want nothing written for a missing object", rec.Body.String())
	}
}
