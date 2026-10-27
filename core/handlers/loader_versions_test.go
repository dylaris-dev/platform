package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type loaderMetadataTransport func(*http.Request) (*http.Response, error)

func (f loaderMetadataTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCompatibleLoaderMetadata(t *testing.T) {
	tests := []struct {
		name, software, mc, body string
		want                     []string
	}{
		{"fabric stable before beta", "fabric", "1.20.1", `[{"loader":{"version":"0.9.0","stable":true}},{"loader":{"version":"0.16.9","stable":true}},{"loader":{"version":"0.20.0-beta.1","stable":false}}]`, []string{"0.16.9", "0.9.0", "0.20.0-beta.1"}},
		{"quilt without stability field", "quilt", "1.20.1", `[{"loader":{"version":"0.20.0-beta.9"}},{"loader":{"version":"0.29.2"}}]`, []string{"0.29.2", "0.20.0-beta.9"}},
		{"forge exact game only", "forge", "1.20.1", `<metadata><versioning><versions><version>1.20.1-47.4.28</version><version>1.20.1-47.4.9</version><version>1.20.2-48.0.1</version></versions></versioning></metadata>`, []string{"47.4.28", "47.4.9"}},
		{"neoforge exact patch only", "neoforge", "1.21.1", `<metadata><versioning><versions><version>21.1.264</version><version>21.0.167</version><version>26.3.0.89</version></versions></versioning></metadata>`, []string{"21.1.264"}},
		{"empty compatibility", "fabric", "1.0", `[]`, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewVersionHandler(&AppState{})
			h.loaderHTTP = &http.Client{Transport: loaderMetadataTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("User-Agent") == "" {
					t.Error("missing user agent")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tt.body)), Header: make(http.Header)}, nil
			})}
			got, err := h.fetchLoaderVersions(context.Background(), tt.software, tt.mc)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			for i, v := range got {
				if v.Version != tt.want[i] {
					t.Errorf("version %d=%q, want %q", i, v.Version, tt.want[i])
				}
			}
			if len(got) > 0 && got[0].Stable && !got[0].Recommended {
				t.Error("first stable build not recommended")
			}
		})
	}
}

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
