package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

func TestVoiceOverviewGroupsByRegion(t *testing.T) {
	relays := []services.VoiceRelay{{ID: "v1", Region: "eu"}, {ID: "v2", Region: "eu"}, {ID: "v3", Region: "us"}}
	ports := []store.VoicePort{
		{ServerID: 1, Region: "eu", Port: 26000, Mod: "svc"},
		{ServerID: 2, Region: "eu", Port: 26001, Mod: "plasmo"},
		{ServerID: 3, Region: "us", Port: 26000, Mod: "svc"},
	}
	usage := []store.VoiceUsageRow{
		{ServerID: 1, ServerName: "a", Region: "eu", RxBytes: 100, TxBytes: 1000},
		{ServerID: 2, ServerName: "b", Region: "eu", RxBytes: 20, TxBytes: 200},
		{ServerID: 3, ServerName: "c", Region: "us", RxBytes: 5, TxBytes: 50},
	}
	ov := buildVoiceOverview("2026-10", relays, ports, usage)

	if len(ov.Regions) != 2 || ov.Regions[0].Region != "eu" || ov.Regions[1].Region != "us" {
		t.Fatalf("regions = %+v, want eu then us", ov.Regions)
	}
	if len(ov.Servers) != 3 {
		t.Fatalf("servers = %+v, want 3", ov.Servers)
	}
	for _, rs := range ov.Regions {
		var rx, tx int64
		for _, s := range ov.Servers {
			if s.Region == rs.Region {
				rx += s.RxBytes
				tx += s.TxBytes
			}
		}
		if rs.RxBytes != rx || rs.TxBytes != tx {
			t.Errorf("%s: region rx/tx %d/%d, servers sum to %d/%d", rs.Region, rs.RxBytes, rs.TxBytes, rx, tx)
		}
		if rs.PortsTotal != 1000 {
			t.Errorf("%s: PortsTotal = %d, want 1000", rs.Region, rs.PortsTotal)
		}
	}
	eu, us := ov.Regions[0], ov.Regions[1]
	if eu.RxBytes != 120 || eu.TxBytes != 1200 || us.RxBytes != 5 || us.TxBytes != 50 {
		t.Errorf("region sums wrong: eu %+v us %+v", eu, us)
	}
	if eu.Relays != 2 || us.Relays != 1 || eu.PortsUsed != 2 || us.PortsUsed != 1 {
		t.Errorf("relay/port counts wrong: eu %+v us %+v", eu, us)
	}
	if s := ov.Servers[1]; s.ServerID != 2 || s.Port != 26001 || s.Mod != "plasmo" {
		t.Errorf("server 2 lost its port row: %+v", s)
	}
}

func TestVoiceOverviewListsIdlePortHolders(t *testing.T) {
	ports := []store.VoicePort{{ServerID: 9, ServerUUID: "u-9", ServerName: "idle", Region: "eu", Port: 26005, Mod: "svc"}}
	ov := buildVoiceOverview("2026-10", nil, ports, nil)
	if len(ov.Servers) != 1 || ov.Servers[0].Port != 26005 || ov.Servers[0].RxBytes != 0 || ov.Servers[0].ServerName != "idle" {
		t.Fatalf("servers = %+v, want the idle holder with zero bytes", ov.Servers)
	}
	if ov.Relays == nil {
		t.Error("relays must encode as [], not null")
	}
}

type voicePoolStore struct {
	store.Store
	cells []store.RegionUsage
}

func (f *voicePoolStore) GetTrafficUsageRegions(string, time.Time) ([]store.RegionUsage, error) {
	return f.cells, nil
}

func (f *voicePoolStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	if scope != services.TrafficScopeDefault {
		return nil, nil
	}
	gb := int64(100)
	return &models.TrafficLimit{IncludedGB: &gb}, nil
}

// Voice rides in the player pool as a subset: the pool reports it, and its
// usedGb is the plain sum of bytes, exactly as before voice existed. Bytes are
// whole GB so a voice share added on top would move usedGb.
func TestFoldTrafficCellsCarriesVoice(t *testing.T) {
	fs := &voicePoolStore{cells: []store.RegionUsage{
		{Region: "eu", Kind: store.TrafficKindEdge, Product: store.TrafficProductBYON, Bytes: 1000 * trafficGB, VoiceBytes: 300 * trafficGB},
		{Region: "eu", Kind: store.TrafficKindEdge, Product: store.TrafficProductRoute, Bytes: 500 * trafficGB, VoiceBytes: 0},
	}}
	h := &BillingHandler{state: &AppState{Store: fs}}
	one := int64(1)
	st := h.trafficStatusFor("u1", &store.UserBilling{MaxNodes: &one})
	if st == nil {
		t.Fatal("no traffic status")
	}
	var edge *trafficPool
	for i := range st.Pools {
		if st.Pools[i].Kind == store.TrafficKindEdge {
			edge = &st.Pools[i]
		}
	}
	if edge == nil {
		t.Fatalf("no edge pool in %+v", st.Pools)
	}
	if edge.VoiceBytes != 300*trafficGB {
		t.Errorf("voiceBytes = %d, want %d", edge.VoiceBytes, 300*trafficGB)
	}
	if edge.UsedGB != 1500 {
		t.Errorf("usedGb = %d, want 1500 - voice must not be added on top", edge.UsedGB)
	}
}

// Core without Redis answers 503, not a nil-pointer panic in the relay lookup.
func TestVoiceOverviewWithoutRedis(t *testing.T) {
	rec := httptest.NewRecorder()
	NewVoiceStatsHandler(&AppState{Store: voiceOverviewStore{}}).GetOverview(rec, httptest.NewRequest(http.MethodGet, "/api/voice/overview", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

type voiceOverviewStore struct{ store.Store }

func (voiceOverviewStore) ListVoicePorts() ([]store.VoicePort, error) { return nil, nil }
func (voiceOverviewStore) ListVoiceUsage(time.Time) ([]store.VoiceUsageRow, error) {
	return nil, nil
}
