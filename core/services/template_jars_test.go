package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTemplateJarReferable(t *testing.T) {
	tests := []struct {
		rel, policy string
		want        bool
	}{
		{"plugins/LuckPerms.jar", TemplatePolicyNoJars, true},
		{"mods/x.JAR", TemplatePolicyNoJars, true},
		{"server.jar", TemplatePolicyNoJars, false}, // the server software
		{"plugins/LuckPerms.jar", TemplatePolicyUploads, false},
		{"plugins/config.yml", TemplatePolicyNoJars, false},
		{"plugins/Geyser/extensions/x.jar", TemplatePolicyNoJars, true},
		{"config/x.jar", TemplatePolicyNoJars, false},
		{"libraries/a/b.jar", TemplatePolicyNoJars, false},
		{"modsx/a.jar", TemplatePolicyNoJars, false},
	}
	for _, tc := range tests {
		if got := TemplateJarReferable(tc.rel, tc.policy); got != tc.want {
			t.Errorf("TemplateJarReferable(%q, %q) = %v, want %v", tc.rel, tc.policy, got, tc.want)
		}
	}
}

// useModrinthServer sends every Modrinth API request to srv.
func useModrinthServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	target, _ := url.Parse(srv.URL)
	prev := modrinthHTTP.Transport
	t.Cleanup(func() { modrinthHTTP.Transport = prev })
	modrinthHTTP.Transport = fakeModrinth(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(r)
	})
}

func TestResolveTemplateJars(t *testing.T) {
	hit, _ := HashTemplateJar("plugins/hit.jar", strings.NewReader("on modrinth"))
	miss, _ := HashTemplateJar("plugins/miss.jar", strings.NewReader("hand made"))
	offCDN, _ := HashTemplateJar("plugins/offcdn.jar", strings.NewReader("elsewhere"))
	wrong512, _ := HashTemplateJar("plugins/wrong.jar", strings.NewReader("sha1 only"))
	jars := []TemplateJar{hit, miss, offCDN, wrong512}

	version := func(id, url, sha1, sha512 string) ModrinthVersion {
		return ModrinthVersion{ID: id, ProjectID: "p-" + id, Files: []ModrinthFile{
			{Filename: "other.jar", URL: modrinthCDNPrefix + "data/x/other.jar", Hashes: map[string]string{"sha1": "00", "sha512": "00"}},
			{Filename: id + ".jar", URL: url, Hashes: map[string]string{"sha1": sha1, "sha512": sha512}},
		}}
	}
	answer := map[string]ModrinthVersion{
		hit.Sha1:      version("v1", modrinthCDNPrefix+"data/p/versions/v1/hit.jar", hit.Sha1, hit.Sha512),
		offCDN.Sha1:   version("v2", "https://example.com/offcdn.jar", offCDN.Sha1, offCDN.Sha512),
		wrong512.Sha1: version("v3", modrinthCDNPrefix+"data/p/versions/v3/w.jar", wrong512.Sha1, "beef"),
	}

	t.Run("hit and misses", func(t *testing.T) {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method != http.MethodPost || r.URL.Path != "/v2/version_files" {
				t.Errorf("request %s %s", r.Method, r.URL.Path)
			}
			json.NewEncoder(w).Encode(answer)
		}))
		t.Cleanup(srv.Close)
		useModrinthServer(t, srv)
		refs, reasons := ResolveTemplateJars(jars)
		if calls != 1 {
			t.Errorf("%d requests, want one for the whole request", calls)
		}
		if len(refs) != 1 || refs[0].Path != hit.Path || refs[0].VersionID != "v1" || refs[0].ProjectID != "p-v1" ||
			refs[0].FileName != "v1.jar" || refs[0].Sha512 != hit.Sha512 || refs[0].SizeBytes != hit.Size {
			t.Fatalf("refs = %+v, want only the hit", refs)
		}
		for _, j := range []TemplateJar{miss, offCDN, wrong512} {
			if reasons[j.Path] != TemplateJarNotOnModrinth {
				t.Errorf("%s: reason %q, want not on Modrinth", j.Path, reasons[j.Path])
			}
		}
		if _, ok := reasons[hit.Path]; ok {
			t.Errorf("the hit was also skipped")
		}
	})

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	for name, srv := range map[string]*httptest.Server{"5xx": down, "unreachable": closed} {
		t.Run(name, func(t *testing.T) {
			useModrinthServer(t, srv)
			refs, reasons := ResolveTemplateJars(jars)
			if len(refs) != 0 {
				t.Errorf("refs %+v from a failed lookup", refs)
			}
			for _, j := range jars {
				if reasons[j.Path] != TemplateJarUnreachable {
					t.Errorf("%s: reason %q, want unreachable", j.Path, reasons[j.Path])
				}
			}
		})
	}
	down.Close()
}

func TestTemplateFileAllowedJarReasons(t *testing.T) {
	for _, c := range []struct{ rel, want string }{
		{"server.jar", templateJarReason},
		{"config/x.jar", templateJarPlaceReason},
		{"libraries/a/b.jar", templateJarPlaceReason},
	} {
		if ok, reason := TemplateFileAllowed(c.rel, TemplatePolicyNoJars); ok || reason != c.want {
			t.Errorf("TemplateFileAllowed(%q) = %v, %q, want %q", c.rel, ok, reason, c.want)
		}
	}
}
