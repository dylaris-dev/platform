package services

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type fakeModrinth func(*http.Request) (*http.Response, error)

func (f fakeModrinth) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// One request for all the jars. A hash Modrinth leaves out is "not on
// Modrinth"; a failed request is "did not answer" for every one of them, and
// must not read as not-found - an operator told a jar is unknown may delete it.
func TestModrinthByHashes(t *testing.T) {
	prev := modrinthHTTP.Transport
	t.Cleanup(func() { modrinthHTTP.Transport = prev })

	calls := 0
	modrinthHTTP.Transport = fakeModrinth(func(r *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v2/version_files" || !strings.Contains(string(body), `"algorithm":"sha1"`) {
			t.Errorf("request %s %s %s", r.Method, r.URL.Path, body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"aaa":{"id":"v1","project_id":"p1","name":"Sodium 0.5"}}`))}, nil
	})
	got, err := ModrinthByHashes([]string{"aaa", "bbb"})
	if err != nil || calls != 1 {
		t.Fatalf("err %v, %d requests: want one request", err, calls)
	}
	if got["aaa"].ProjectID != "p1" {
		t.Fatalf("hit = %+v", got["aaa"])
	}
	if _, ok := got["bbb"]; ok {
		t.Fatal("a hash Modrinth did not return came back as found")
	}

	modrinthHTTP.Transport = fakeModrinth(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("down"))}, nil
	})
	if _, err := ModrinthByHashes([]string{"aaa"}); err == nil || ModrinthNotFound(err) {
		t.Fatalf("err = %v: a Modrinth outage must be an error and not a not-found", err)
	}

	modrinthHTTP.Transport = fakeModrinth(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	calls = 0
	if _, err := ModrinthByHashes(make([]string, modrinthHashBatch+1)); err != nil || calls != 2 {
		t.Fatalf("err %v, %d requests for %d hashes: want two batches", err, calls, modrinthHashBatch+1)
	}
	calls = 0
	if _, err := ModrinthByHashes(nil); err != nil || calls != 0 {
		t.Fatalf("no hashes made %d requests", calls)
	}
}
