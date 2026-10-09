package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

func intp(n int) *int { return &n }

// --- resources PATCH ---

// sentDocker runs one admin PATCH against a queue and returns the docker
// object the node was sent.
func sentDocker(t *testing.T, fake *resourcesFakeStore, body map[string]any) map[string]interface{} {
	t.Helper()
	rdb := resourcesRedis(t)
	h := &ServerHandler{state: &AppState{Store: fake, Queue: services.NewQueueService(rdb)}}
	rec := httptest.NewRecorder()
	h.UpdateServerResources(rec, resourcesReq(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	cmd := readNodeCmdStream(t, rdb, "dylaris:node:node-tok-7:cmds")
	cfg, _ := cmd["config"].(map[string]interface{})
	docker, _ := cfg["docker"].(map[string]interface{})
	return docker
}

// sentPaddingMB is sentDocker for a save that must carry ramPaddingMB.
func sentPaddingMB(t *testing.T, fake *resourcesFakeStore, body map[string]any) float64 {
	t.Helper()
	docker := sentDocker(t, fake, body)
	got, ok := docker["ramPaddingMB"].(float64)
	if !ok {
		t.Fatalf("payload has no ramPaddingMB: %v", docker)
	}
	return got
}

// resourcesServer4G is a server already booked at the 4096 MB the tests send,
// so the RAM itself does not change.
func resourcesServer4G() *models.Server {
	srv := resourcesServer("online")
	srv.Memory = 4096
	return srv
}

func TestUpdateServerResources_RAMPaddingOverrideReachesStoreAndNode(t *testing.T) {
	fake := &resourcesFakeStore{server: resourcesServer4G(), node: models.Node{RAMPaddingMB: intp(1024)}}
	got := sentPaddingMB(t, fake, map[string]any{"ram": 4096, "cpuLimit": 2.0, "diskLimit": 10240, "ramPaddingMb": 2048})
	if got != 2048 {
		t.Errorf("node got ramPaddingMB %v, want the 2048 override", got)
	}
	if !fake.paddingWritten || fake.padding == nil || *fake.padding != 2048 {
		t.Errorf("store write = %v/%v, want 2048", fake.paddingWritten, fake.padding)
	}
}

func TestUpdateServerResources_ResetRAMPaddingClearsTheOverride(t *testing.T) {
	srv := resourcesServer4G()
	srv.RAMPaddingMB = intp(2048)
	fake := &resourcesFakeStore{server: srv, settings: map[string]string{models.RAMPaddingSetting: "768"}}
	got := sentPaddingMB(t, fake, map[string]any{"ram": 4096, "cpuLimit": 2.0, "diskLimit": 10240, "resetRamPadding": true})
	if !fake.paddingWritten || fake.padding != nil {
		t.Errorf("store write = %v/%v, want a cleared (nil) override", fake.paddingWritten, fake.padding)
	}
	if got != 768 {
		t.Errorf("node got ramPaddingMB %v, want the global 768 after the reset", got)
	}
}

// A save that touches neither RAM nor padding must not send the padding: a
// node default changed since would make the node recreate (restart) a running
// server on a CPU-only save, unconfirmed.
func TestUpdateServerResources_NoPaddingFieldsAndSameRAMSendsNoPadding(t *testing.T) {
	fake := &resourcesFakeStore{server: resourcesServer4G(), node: models.Node{RAMPaddingMB: intp(1536)}}
	docker := sentDocker(t, fake, map[string]any{"ram": 4096, "cpuLimit": 3.0, "diskLimit": 10240})
	if fake.paddingWritten {
		t.Error("an absent padding field wrote the override")
	}
	if v, ok := docker["ramPaddingMB"]; ok {
		t.Errorf("a CPU-only save sent ramPaddingMB %v", v)
	}
}

// A RAM change recreates the container anyway, so it carries the effective
// padding and a changed node/global default lands with it.
func TestUpdateServerResources_RAMChangeSendsEffectivePadding(t *testing.T) {
	fake := &resourcesFakeStore{server: resourcesServer4G(), node: models.Node{RAMPaddingMB: intp(1536)}}
	got := sentPaddingMB(t, fake, map[string]any{"ram": 6144, "cpuLimit": 2.0, "diskLimit": 10240})
	if fake.paddingWritten {
		t.Error("an absent padding field wrote the override")
	}
	if got != 1536 {
		t.Errorf("node got ramPaddingMB %v, want the node's 1536", got)
	}
}

func TestUpdateServerResources_InvalidRAMPaddingWritesNothing(t *testing.T) {
	bodies := map[string]string{
		"both set and reset": `{"ram":4096,"cpuLimit":2,"diskLimit":10240,"ramPaddingMb":1024,"resetRamPadding":true}`,
		"negative":           `{"ram":4096,"cpuLimit":2,"diskLimit":10240,"ramPaddingMb":-1}`,
		"too large":          `{"ram":4096,"cpuLimit":2,"diskLimit":10240,"ramPaddingMb":16385}`,
		"not a number":       `{"ram":4096,"cpuLimit":2,"diskLimit":10240,"ramPaddingMb":"512"}`,
		"fraction":           `{"ram":4096,"cpuLimit":2,"diskLimit":10240,"ramPaddingMb":1.5}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			fake := &resourcesFakeStore{server: resourcesServer("online")}
			h := &ServerHandler{state: &AppState{Store: fake}}
			r := resourcesReq(t, nil)
			r.Body = io.NopCloser(strings.NewReader(body))
			rec := httptest.NewRecorder()
			h.UpdateServerResources(rec, r)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if fake.resourcesWritten || fake.paddingWritten || fake.pinningWritten || fake.portsWritten {
				t.Error("a refused request still wrote")
			}
		})
	}
}

type nonAdminResourcesStore struct{ *resourcesFakeStore }

func (nonAdminResourcesStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, Role: "user"}, nil
}

// The padding is memory the host gives the container, so it sits behind the
// same flag as RAM.
func TestUpdateServerResources_RAMPaddingNeedsTheResourcesFlag(t *testing.T) {
	fake := &resourcesFakeStore{server: resourcesServer("online")}
	h := &ServerHandler{state: &AppState{Store: nonAdminResourcesStore{fake}}}
	r := resourcesReq(t, map[string]any{"ram": 4096, "cpuLimit": 2.0, "diskLimit": 10240, "ramPaddingMb": 4096})
	r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "isAdmin", false), "userID", "user-1"))
	rec := httptest.NewRecorder()
	h.UpdateServerResources(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if fake.paddingWritten {
		t.Error("padding written without can_change_resources")
	}
}

// --- global setting ---

type paddingSettingsStore struct {
	store.Store
	settings map[string]string
	written  map[string]string
}

func (f *paddingSettingsStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *paddingSettingsStore) SetSetting(k, v string) error {
	f.written[k] = v
	return nil
}

func savePlacement(t *testing.T, f *paddingSettingsStore, extra string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"cpuOvercommitDefault":2,"ramOvercommitDefault":1` + extra + `}`
	rec := httptest.NewRecorder()
	(&SettingsHandler{state: &AppState{Store: f}}).SavePlacementSettings(rec, httptest.NewRequest(http.MethodPost, "/api/settings/placement", strings.NewReader(body)))
	return rec
}

func TestSavePlacementSettings_RAMPadding(t *testing.T) {
	for _, bad := range []string{`-1`, `16385`, `"abc"`, `1.5`} {
		f := &paddingSettingsStore{written: map[string]string{}}
		rec := savePlacement(t, f, `,"ramPaddingMb":`+bad)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("ramPaddingMb %s: status %d, want 400", bad, rec.Code)
		}
		if len(f.written) != 0 {
			t.Errorf("ramPaddingMb %s: wrote %v on a refused request", bad, f.written)
		}
	}

	f := &paddingSettingsStore{written: map[string]string{}}
	if rec := savePlacement(t, f, `,"ramPaddingMb":0`); rec.Code != http.StatusOK {
		t.Fatalf("ramPaddingMb 0: status %d: %s", rec.Code, rec.Body.String())
	}
	if v, ok := f.written[models.RAMPaddingSetting]; !ok || v != "0" {
		t.Errorf("ramPaddingMb 0 stored as %q (present %v), want \"0\"", v, ok)
	}

	// An older panel does not send the field; that must not save 0.
	f = &paddingSettingsStore{written: map[string]string{}}
	if rec := savePlacement(t, f, ``); rec.Code != http.StatusOK {
		t.Fatalf("absent: status %d", rec.Code)
	}
	if _, ok := f.written[models.RAMPaddingSetting]; ok {
		t.Error("an absent ramPaddingMb overwrote the stored global")
	}
}

func TestLoadPlacementSettings_RAMPaddingDefaults(t *testing.T) {
	for raw, want := range map[string]int{"": 512, "garbage": 512, "1024": 1024, "0": 0} {
		f := &paddingSettingsStore{settings: map[string]string{models.RAMPaddingSetting: raw}}
		if got := (&SettingsHandler{state: &AppState{Store: f}}).LoadPlacementSettings().RAMPaddingMB; got != want {
			t.Errorf("stored %q: ramPaddingMb %d, want %d", raw, got, want)
		}
	}
}

// --- node placement ---

type nodePaddingStore struct {
	store.Store
	placementWritten bool
	paddingWritten   bool
	padding          *int
}

func (f *nodePaddingStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id}, nil
}
func (f *nodePaddingStore) SetNodePlacement(int, float64, float64) error {
	f.placementWritten = true
	return nil
}
func (f *nodePaddingStore) SetNodeRAMPadding(_ int, mb *int) error {
	f.paddingWritten = true
	f.padding = mb
	return nil
}

func TestSetNodePlacement_RAMPaddingNullVsAbsent(t *testing.T) {
	cases := []struct {
		name        string
		extra       string
		wantCode    int
		wantWritten bool
		wantPadding *int
	}{
		{"absent leaves it alone", ``, 200, false, nil},
		{"null clears it", `,"ramPaddingMb":null`, 200, true, nil},
		{"number sets it", `,"ramPaddingMb":768`, 200, true, intp(768)},
		{"zero is a real value", `,"ramPaddingMb":0`, 200, true, intp(0)},
		{"negative refused", `,"ramPaddingMb":-5`, 400, false, nil},
		{"too large refused", `,"ramPaddingMb":20000`, 400, false, nil},
		{"string refused", `,"ramPaddingMb":"768"`, 400, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &nodePaddingStore{}
			r := httptest.NewRequest(http.MethodPut, "/api/nodes/7/placement",
				strings.NewReader(`{"cpuOvercommitRatio":1,"ramOvercommitRatio":1`+c.extra+`}`))
			rec := httptest.NewRecorder()
			(&PlacementHandler{state: &AppState{Store: f}}).SetNodePlacement(rec, r)
			if rec.Code != c.wantCode {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.wantCode, rec.Body.String())
			}
			if c.wantCode != 200 && f.placementWritten {
				t.Error("a refused padding still saved the ratios")
			}
			if f.paddingWritten != c.wantWritten {
				t.Fatalf("padding written = %v, want %v", f.paddingWritten, c.wantWritten)
			}
			if (f.padding == nil) != (c.wantPadding == nil) || (f.padding != nil && *f.padding != *c.wantPadding) {
				t.Errorf("padding = %v, want %v", f.padding, c.wantPadding)
			}
		})
	}
}

// --- pick node ---

// A negative padding shrinks the request until every node fits.
func TestPickNode_RefusesInvalidRAMPadding(t *testing.T) {
	h := newPlacementHandler(t, nil)
	for _, bad := range []string{`-4096`, `16385`, `"512"`} {
		rec := httptest.NewRecorder()
		h.PickNode(rec, httptest.NewRequest(http.MethodPost, "/api/placement/pick",
			strings.NewReader(`{"ramMb":1024,"ramPaddingMb":`+bad+`}`)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("ramPaddingMb %s: status %d, want 400", bad, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.PickNode(rec, httptest.NewRequest(http.MethodPost, "/api/placement/pick",
		strings.NewReader(`{"ramMb":1024,"ramPaddingMb":0}`)))
	if rec.Code != http.StatusOK {
		t.Errorf("ramPaddingMb 0: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// --- capacity ---

// A node with exactly booked RAM + padding left fits; the same request on a
// node whose own padding is larger does not, because the container is bigger.
func TestScoreNode_CountsTheCandidatesPadding(t *testing.T) {
	h := newPlacementHandler(t, nil)
	node := ownedNode(1, "n1", nil)
	node.TotalRAMMB = 4096

	if c := h.scoreNode(context.Background(), &node, PickNodeRequest{RAMMB: 3584}, nil); !c.Available {
		t.Fatalf("3584 + global 512 on 4096 should fit: %s", c.Reason)
	}
	if c := h.scoreNode(context.Background(), &node, PickNodeRequest{RAMMB: 3585}, nil); c.Available {
		t.Fatal("3585 + global 512 on 4096 fit; the padding is not counted")
	}
	node.RAMPaddingMB = intp(1024)
	if c := h.scoreNode(context.Background(), &node, PickNodeRequest{RAMMB: 3584}, nil); c.Available {
		t.Fatal("3584 + node padding 1024 on 4096 fit")
	}
	if c := h.scoreNode(context.Background(), &node, PickNodeRequest{RAMMB: 3584, RAMPaddingMB: intp(0)}, nil); !c.Available {
		t.Fatalf("a server override of 0 should win over the node's 1024: %s", c.Reason)
	}
}
