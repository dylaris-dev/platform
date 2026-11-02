package modcompat

import (
	"context"
	"io"
	"net/http"
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
			c := &http.Client{Transport: loaderMetadataTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("User-Agent") == "" {
					t.Error("missing user agent")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tt.body)), Header: make(http.Header)}, nil
			})}
			got, err := FetchLoaderVersions(context.Background(), c, tt.software, tt.mc)
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

// The packs migration pins this answer into an exported build, so it must be
// the first STABLE build in picker order, and "none" must be an error rather
// than an empty version.
func TestPreferredLoaderVersionIsTheFirstStableBuild(t *testing.T) {
	serve := func(body string) *http.Client {
		return &http.Client{Transport: loaderMetadataTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}
	}
	got, err := PreferredLoaderVersion(context.Background(), serve(`[{"loader":{"version":"0.20.0-beta.1","stable":false}},{"loader":{"version":"0.16.9","stable":true}}]`), "fabric", "1.20.1")
	if err != nil || got != "0.16.9" {
		t.Fatalf("got %q, %v; want 0.16.9", got, err)
	}
	if got, err := PreferredLoaderVersion(context.Background(), serve(`[{"loader":{"version":"0.20.0-beta.1","stable":false}}]`), "fabric", "1.20.1"); err == nil {
		t.Fatalf("no stable build answered %q without an error", got)
	}
}
