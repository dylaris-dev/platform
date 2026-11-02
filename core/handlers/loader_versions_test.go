package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type loaderMetadataTransport func(*http.Request) (*http.Response, error)

func (f loaderMetadataTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLoaderMetadataFailuresAreNotEmptySuccess(t *testing.T) {
	h := NewVersionHandler(&AppState{})
	h.loaderHTTP = &http.Client{Transport: loaderMetadataTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})}
	for _, tc := range []struct {
		query  string
		status int
	}{{"software=fabric&minecraft=1.20.1", 502}, {"software=fabric&minecraft=../x", 400}, {"software=unknown&minecraft=1.20.1", 400}} {
		r := httptest.NewRequest(http.MethodGet, "/api/versions/loaders?"+tc.query, nil)
		w := httptest.NewRecorder()
		h.GetLoaderVersions(w, r)
		if w.Code != tc.status {
			t.Errorf("%s status=%d want %d", tc.query, w.Code, tc.status)
		}
	}
}
