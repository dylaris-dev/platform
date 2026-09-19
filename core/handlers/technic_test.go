package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/services"
)

// technicUpstream fakes api.technicpack.net and one Solder server. Solder is
// served by the same httptest server under /solder/; the handler's external
// fetch is swapped for a plain GET because the real one refuses loopback.
type technicUpstream struct {
	srv    *httptest.Server
	calls  atomic.Int32
	status atomic.Int32 // non-zero forces this status on the API
	packs  map[string]string
}

func newTechnicUpstream(t *testing.T) *technicUpstream {
	t.Helper()
	u := &technicUpstream{packs: map[string]string{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/solder/") {
			switch r.URL.Path {
			case "/solder/modpack/solderpack":
				io.WriteString(w, `{"recommended":"1.0.1","latest":"1.0.2","builds":["1.0.2","1.0.1"]}`)
			case "/solder/modpack/solderpack/1.0.1":
				io.WriteString(w, `{"minecraft":"1.16.5","mods":[{"name":"forge","url":"https://cdn.example/forge.zip","md5":"abc"}]}`)
			case "/solder/modpack/solderpack/1.0.2":
				io.WriteString(w, `{"minecraft":"1.16.5","mods":[{"name":"x","url":"file:///etc/passwd","md5":""}]}`)
			default:
				http.NotFound(w, r)
			}
			return
		}
		u.calls.Add(1)
		if r.URL.Query().Get("build") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if s := u.status.Load(); s != 0 {
			w.Header().Set("Retry-After", "40")
			w.WriteHeader(int(s))
			return
		}
		if r.URL.Path == "/search" {
			io.WriteString(w, `{"modpacks":[{"name":"Tekkit","slug":"tekkit","iconUrl":"https://cdn/x.png"},{"name":"Bad","slug":"../etc","iconUrl":""}]}`)
			return
		}
		slug := strings.TrimPrefix(r.URL.Path, "/modpack/")
		body, ok := u.packs[slug]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"Modpack does not exist"}`)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(u.srv.Close)
	u.packs["tekkit"] = `{"name":"tekkit","displayName":"Tekkit Classic","user":"sct","url":null,"minecraft":"1.4.7","version":"1.0","solder":"` + u.srv.URL + `/solder/","serverPackUrl":"https://servers.example/Tekkit_Server.zip","icon":{"url":"https://cdn/t.png"},"platformUrl":"https://www.technicpack.net/modpack/tekkit.552560"}`
	u.packs["zippack"] = `{"name":"zippack","minecraft":"1.21.1","version":"2.3","url":"https://cdn.example/pack.zip","solder":null}`
	u.packs["solderpack"] = `{"name":"solderpack","minecraft":"1.12.2","version":"9","url":null,"solder":"` + u.srv.URL + `/solder/"}`
	u.packs["nothing"] = `{"name":"nothing","url":null,"solder":null,"serverPackUrl":""}`
	u.packs["ftp"] = `{"name":"ftp","url":"ftp://host/pack.zip"}`
	return u
}

func newTestTechnicHandler(t *testing.T, u *technicUpstream) *TechnicHandler {
	t.Helper()
	technicCooldown = upstreamCooldown{}
	t.Cleanup(func() { technicCooldown = upstreamCooldown{} })
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	h := NewTechnicHandler(&AppState{Cache: services.NewCache(rdb)})
	h.base = u.srv.URL
	h.fetchExternal = func(ctx context.Context, raw string, max int64, _ time.Duration) ([]byte, error) {
		resp, err := http.Get(raw)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, errors.New(resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	return h
}

func TestResolveTechnic(t *testing.T) {
	u := newTechnicUpstream(t)
	h := newTestTechnicHandler(t, u)
	ctx := context.Background()

	cases := []struct {
		slug, build string
		wantVariant string
		wantURL     string
		wantMods    int
		wantErr     string
	}{
		{slug: "tekkit", wantVariant: "server", wantURL: "https://servers.example/Tekkit_Server.zip"},
		{slug: "zippack", wantVariant: "client-zip", wantURL: "https://cdn.example/pack.zip"},
		{slug: "solderpack", wantVariant: "client-solder", wantMods: 1},
		{slug: "solderpack", build: "1.0.1", wantVariant: "client-solder", wantMods: 1},
		{slug: "solderpack", build: "7.7.7", wantErr: "build does not exist"},
		{slug: "solderpack", build: "1.0.2", wantErr: "not an http(s) URL"},
		{slug: "nothing", wantErr: "no download"},
		{slug: "ftp", wantErr: "not an http(s) URL"},
		{slug: "../etc", wantErr: "Invalid Technic pack name"},
		{slug: "missing", wantErr: "technic pack not found"},
	}
	for _, c := range cases {
		t.Run(c.slug+"/"+c.build, func(t *testing.T) {
			res, err := h.resolveTechnic(ctx, c.slug, c.build)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Variant != c.wantVariant || res.URL != c.wantURL || len(res.Mods) != c.wantMods {
				t.Errorf("got %+v", res)
			}
			if c.wantVariant == "client-solder" && (res.MCVersion != "1.16.5" || res.Build != "1.0.1") {
				t.Errorf("solder build/mc = %q/%q, want the recommended build's", res.Build, res.MCVersion)
			}
		})
	}
}

func TestTechnicCooldown(t *testing.T) {
	u := newTechnicUpstream(t)
	h := newTestTechnicHandler(t, u)
	ctx := context.Background()

	// Warm the cache for one pack, then let Technic start refusing.
	if _, err := h.fetchPack(ctx, "tekkit"); err != nil {
		t.Fatal(err)
	}
	u.status.Store(http.StatusTooManyRequests)

	_, err := h.fetchPack(ctx, "zippack")
	var cool technicCoolingDown
	if !errors.As(err, &cool) || cool.left != 40*time.Second {
		t.Fatalf("err = %v, want a 40s cooldown from Retry-After", err)
	}
	before := u.calls.Load()
	if _, err := h.fetchPack(ctx, "zippack"); !errors.As(err, &cool) {
		t.Fatalf("second call: err = %v, want the cooldown", err)
	}
	if u.calls.Load() != before {
		t.Error("Technic was called again during the cooldown")
	}
	// A cached pack still answers while Technic is cooling down.
	if _, err := h.fetchPack(ctx, "tekkit"); err != nil {
		t.Errorf("cached pack during cooldown: %v", err)
	}

	rec := httptest.NewRecorder()
	sendTechnicError(rec, err)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("cooldown answer = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestTechnicRoutes(t *testing.T) {
	u := newTechnicUpstream(t)
	h := newTestTechnicHandler(t, u)
	r := mux.NewRouter()
	r.HandleFunc("/api/technic/search", h.Search)
	r.HandleFunc("/api/technic/pack/{slug}", h.Pack)

	get := func(path string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}

	code, m := get("/api/technic/search?q=tekkit")
	packs, _ := m["packs"].([]any)
	if code != 200 || len(packs) != 1 {
		t.Fatalf("search = %d %v, want the one valid slug", code, m)
	}
	before := u.calls.Load()
	if code, m := get("/api/technic/search?q="); code != 200 || len(m["packs"].([]any)) != 0 || u.calls.Load() != before {
		t.Errorf("empty search = %d %v, want an empty list without calling Technic", code, m)
	}

	code, m = get("/api/technic/pack/tekkit")
	if code != 200 || m["variant"] != "server" || m["iconUrl"] != "https://cdn/t.png" || m["builds"] != nil {
		t.Errorf("pack = %d %v", code, m)
	}
	code, m = get("/api/technic/pack/solderpack")
	b, _ := m["builds"].(map[string]any)
	if code != 200 || m["variant"] != "client-solder" || b["recommended"] != "1.0.1" {
		t.Errorf("solder pack = %d %v", code, m)
	}
	if code, _ := get("/api/technic/pack/missing"); code != 404 {
		t.Errorf("missing pack = %d, want 404", code)
	}
	if code, _ := get("/api/technic/pack/Bad_Slug"); code != 400 {
		t.Errorf("bad slug = %d, want 400", code)
	}
}
