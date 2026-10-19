package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// bulkHandler returns a handler whose upstream calls are recorded and answered
// with reply, never sent anywhere.
func bulkHandler(reply string) (*ModrinthHandler, *[]*http.Request, *[]string) {
	var reqs []*http.Request
	var bodies []string
	h := NewModrinthHandler(&AppState{})
	h.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reqs = append(reqs, r)
		b := ""
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			b = string(raw)
		}
		bodies = append(bodies, b)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(reply))}, nil
	})}
	return h, &reqs, &bodies
}

var (
	sha512A = strings.Repeat("a", 128)
	sha1A   = strings.Repeat("b", 40)
)

func hashesBody(hashes []string, rest string) string {
	raw, _ := json.Marshal(hashes)
	return `{"hashes":` + string(raw) + rest + `}`
}

func TestLatestFromHashesValidation(t *testing.T) {
	many := make([]string, modrinthBulkMaxHashes+1)
	for i := range many {
		many[i] = sha512A
	}
	ok := `,"algorithm":"sha512","loaders":["fabric"],"game_versions":["1.21.1"]`
	cases := []struct {
		name string
		body string
	}{
		{"not json", `{`},
		{"bad algorithm", hashesBody([]string{sha512A}, `,"algorithm":"md5","loaders":["fabric"],"game_versions":["1.21.1"]`)},
		{"sha1 length under sha512", hashesBody([]string{sha1A}, ok)},
		{"uppercase hash", hashesBody([]string{strings.ToUpper(sha512A)}, ok)},
		{"no hashes", hashesBody([]string{}, ok)},
		{"too many hashes", hashesBody(many, ok)},
		{"no loaders", hashesBody([]string{sha512A}, `,"algorithm":"sha512","loaders":[],"game_versions":["1.21.1"]`)},
		{"bad loader", hashesBody([]string{sha512A}, `,"algorithm":"sha512","loaders":["Fab ric"],"game_versions":["1.21.1"]`)},
		{"no game versions", hashesBody([]string{sha512A}, `,"algorithm":"sha512","loaders":["fabric"]`)},
		{"bad game version", hashesBody([]string{sha512A}, `,"algorithm":"sha512","loaders":["fabric"],"game_versions":["1.21/../x"]`)},
		{"bad version type", hashesBody([]string{sha512A}, ok+`,"version_types":["snapshot"]`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, reqs, _ := bulkHandler(`{}`)
			rec := httptest.NewRecorder()
			h.LatestFromHashes(rec, httptest.NewRequest(http.MethodPost, "/api/modrinth/version-files/update", strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if len(*reqs) != 0 {
				t.Errorf("an invalid request reached Modrinth")
			}
		})
	}
	// The same shape with valid values passes, so the cases above fail on the
	// field they name and not on something shared.
	h, reqs, _ := bulkHandler(`{}`)
	rec := httptest.NewRecorder()
	h.LatestFromHashes(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(hashesBody([]string{sha512A}, ok+`,"version_types":["release","beta"]`))))
	if rec.Code != 200 || len(*reqs) != 1 {
		t.Errorf("valid request: status %d, upstream calls %d", rec.Code, len(*reqs))
	}
}

func TestLatestFromHashesBodyCap(t *testing.T) {
	h, reqs, _ := bulkHandler(`{}`)
	// A request that is valid in every field and only too big: without the cap
	// it would be decoded and forwarded.
	body := hashesBody([]string{sha512A}, `,"algorithm":"sha512","loaders":["fabric"],"game_versions":["1.21.1"]`+strings.Repeat(" ", modrinthBulkBodyLimit))
	rec := httptest.NewRecorder()
	h.LatestFromHashes(rec, httptest.NewRequest(http.MethodPost, "/api/modrinth/version-files/update", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest || len(*reqs) != 0 {
		t.Errorf("oversized body: status %d, upstream calls %d; want 400 and none", rec.Code, len(*reqs))
	}
}

func TestLatestFromHashesForwardsOnlyToModrinth(t *testing.T) {
	h, reqs, bodies := bulkHandler(`{"` + sha512A + `":{"id":"v2","changelog":"long text","version_type":"release"}}`)
	in := hashesBody([]string{sha512A}, `,"algorithm":"sha512","loaders":["fabric"],"game_versions":["1.21.1"],"version_types":["release"],"extra":"dropped"`)
	rec := httptest.NewRecorder()
	h.LatestFromHashes(rec, httptest.NewRequest(http.MethodPost, "/api/modrinth/version-files/update", strings.NewReader(in)))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(*reqs) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(*reqs))
	}
	r := (*reqs)[0]
	if r.Method != http.MethodPost || r.URL.String() != "https://api.modrinth.com/v2/version_files/update" {
		t.Errorf("upstream = %s %s", r.Method, r.URL)
	}
	if strings.Contains((*bodies)[0], "extra") || !strings.Contains((*bodies)[0], `"version_types":["release"]`) {
		t.Errorf("upstream body = %s, want the validated fields only", (*bodies)[0])
	}
	if strings.Contains(rec.Body.String(), "changelog") || !strings.Contains(rec.Body.String(), `"id":"v2"`) {
		t.Errorf("response = %s, want the version without its changelog", rec.Body.String())
	}
}

func TestBulkIDRoutes(t *testing.T) {
	ids101 := make([]string, modrinthBulkMaxIDs+1)
	for i := range ids101 {
		ids101[i] = fmt.Sprintf("id%d", i)
	}
	versions := func(h *ModrinthHandler) http.HandlerFunc { return h.Versions }
	projects := func(h *ModrinthHandler) http.HandlerFunc { return h.Projects }
	cases := []struct {
		name    string
		handler func(*ModrinthHandler) http.HandlerFunc
		ids     string
		want    int
		wantURL string
	}{
		{"versions, deduped", versions, "AbC123,xyz,AbC123", 200, `https://api.modrinth.com/v2/versions?ids=%5B%22AbC123%22%2C%22xyz%22%5D`},
		{"projects", projects, "AANobbMI", 200, `https://api.modrinth.com/v2/projects?ids=%5B%22AANobbMI%22%5D`},
		{"exactly the cap", versions, strings.Join(ids101[:modrinthBulkMaxIDs], ","), 200, ""},
		{"empty", versions, "", 400, ""},
		{"path in id", versions, "../user", 400, ""},
		{"quote in id", projects, `a"]`, 400, ""},
		{"over the cap", versions, strings.Join(ids101, ","), 400, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, reqs, _ := bulkHandler(`[{"id":"p","title":"Sodium","body":"huge","changelog":"c"}]`)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			q := req.URL.Query()
			q.Set("ids", tc.ids)
			req.URL.RawQuery = q.Encode()
			tc.handler(h)(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want != 200 {
				if len(*reqs) != 0 {
					t.Error("an invalid request reached Modrinth")
				}
				return
			}
			if len(*reqs) != 1 || (*reqs)[0].URL.Host != "api.modrinth.com" {
				t.Fatalf("upstream = %v, want one call to api.modrinth.com", *reqs)
			}
			if tc.wantURL != "" && (*reqs)[0].URL.String() != tc.wantURL {
				t.Errorf("upstream = %s, want %s", (*reqs)[0].URL, tc.wantURL)
			}
			if bytes.Contains(rec.Body.Bytes(), []byte("changelog")) {
				t.Errorf("response kept the changelog: %s", rec.Body.String())
			}
			if strings.Contains(tc.wantURL, "/projects") && bytes.Contains(rec.Body.Bytes(), []byte("huge")) {
				t.Errorf("response kept a stripped field: %s", rec.Body.String())
			}
		})
	}
}

func TestSlimProjectsKeepsClientSide(t *testing.T) {
	in := []byte(`[{"id":"P1","slug":"sodium","title":"Sodium","client_side":"required","server_side":"unsupported","body":"long text","gallery":[]}]`)
	var out []map[string]interface{}
	if err := json.Unmarshal(slimProjects(in), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0]["client_side"] != "required" || out[0]["server_side"] != "unsupported" || out[0]["slug"] != "sodium" {
		t.Errorf("slim = %v, want slug, client_side and server_side kept", out)
	}
	if _, ok := out[0]["body"]; ok {
		t.Errorf("slim kept body: %v", out)
	}
}

func TestSideFacets(t *testing.T) {
	cases := []struct {
		side string
		want string
	}{
		{"client", `[["client_side:required","client_side:optional"],["server_side:optional","server_side:unsupported"]]`},
		{"server", `[["server_side:required","server_side:optional"],["client_side:optional","client_side:unsupported"]]`},
		{"both", `[["client_side:required","client_side:optional"],["server_side:required","server_side:optional"]]`},
		{"", `null`},
		{"all", `null`},
	}
	for _, tc := range cases {
		got, _ := json.Marshal(sideFacets(tc.side))
		if string(got) != tc.want {
			t.Errorf("sideFacets(%q) = %s, want %s", tc.side, got, tc.want)
		}
	}
}
