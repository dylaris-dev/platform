package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestGatewayBandwidthGetHistory_ShapeAndRange(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	st := store.NewPostgresStore(db)

	t0 := time.Unix(1730000000, 0)
	mock.ExpectQuery("SELECT time, component, id, host, region, rx_bps, tx_bps, cap_mbit").
		WillReturnRows(sqlmock.NewRows([]string{"time", "component", "id", "host", "region", "rx_bps", "tx_bps", "cap_mbit"}).
			AddRow(t0, "warp", "eu-1", "web-eu-1", "eu", int64(100), int64(200), 1000))

	h := NewGatewayBandwidthHandler(&handlers.AppState{Store: st})
	req := httptest.NewRequest(http.MethodGet, "/api/gateway-bandwidth/history?range=6h&host=web-eu-1", nil)
	rw := httptest.NewRecorder()
	h.GetHistory(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	var body services.BandwidthHistory
	if err := json.NewDecoder(rw.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.StepSec != 300 {
		t.Fatalf("stepSec = %d, want 300 for range=6h", body.StepSec)
	}
	if len(body.Components) != 1 || body.Components[0].Component != "warp" {
		t.Fatalf("unexpected component series: %+v", body.Components)
	}
	if len(body.Hosts) != 1 || len(body.Hosts[0].Points) != 1 || body.Hosts[0].Points[0].TxBps != 200 {
		t.Fatalf("unexpected host series: %+v", body.Hosts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// The window a range name asks for, and the bucket its points land on.
//
// 24h used to be the ceiling, and this test used to assert exactly that: the
// raw rows are kept for a day, so a longer window came back empty at its left
// edge and looked like an outage. That is no longer the whole story - past the
// raw retention GetHistory reads the long-term record instead - so the
// assertion moved from "nothing exceeds a day" to "anything that exceeds a day
// is answered from the other source".
func TestBandwidthRangeWindowsAndSteps(t *testing.T) {
	cases := []struct {
		name   string
		window time.Duration
		step   time.Duration
	}{
		{"15m", 15 * time.Minute, 0},
		{"1h", time.Hour, time.Minute},
		{"6h", 6 * time.Hour, 5 * time.Minute},
		{"12h", 12 * time.Hour, 10 * time.Minute},
		{"24h", 24 * time.Hour, 15 * time.Minute},
		{"7d", 7 * 24 * time.Hour, 2 * time.Hour},
		{"30d", 30 * 24 * time.Hour, 6 * time.Hour},
		// An unknown name is the panel's own switcher sending something this
		// build does not know, so it lands on the day rather than erroring.
		{"", 24 * time.Hour, 15 * time.Minute},
		{"nonsense", 24 * time.Hour, 15 * time.Minute},
	}
	for _, c := range cases {
		w, s := bandwidthRange(c.name)
		if w != c.window || s != c.step {
			t.Errorf("range %q = (%v, %v), want (%v, %v)", c.name, w, s, c.window, c.step)
		}
	}
	// The shortest range is raw: the persist cadence is 30s, so there is
	// nothing to reduce, and this is the range somebody opens to watch a spike.
	if _, s := bandwidthRange("15m"); s != 0 {
		t.Errorf("15m bucketed at %v; it is meant to be raw", s)
	}
	// Every long window has to bucket, or it asks the long-term store for a
	// week at its native minute and gets ten thousand points per component.
	for _, long := range []string{"7d", "30d"} {
		w, s := bandwidthRange(long)
		if w <= services.RawRetention {
			t.Errorf("range %q is inside the raw retention; it would read the wrong source", long)
		}
		if s < time.Hour {
			t.Errorf("range %q buckets at %v, fine enough to return thousands of points per component", long, s)
		}
	}
}

// A window past the raw retention must not be answered from the raw table. The
// store would return nothing for it (the rows are deleted at a day) and the
// screen would show an empty week as though there had been no traffic.
func TestALongWindowDoesNotTouchTheRawTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// No ExpectQuery at all: any query against the store fails the mock.
	h := NewGatewayBandwidthHandler(&handlers.AppState{Store: store.NewPostgresStore(db)})
	rw := httptest.NewRecorder()
	h.GetHistory(rw, httptest.NewRequest(http.MethodGet, "/api/gateway-bandwidth/history?range=7d", nil))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	var body struct {
		StepSec    int    `json:"stepSec"`
		Components []any  `json:"components"`
		Note       string `json:"note"`
	}
	if err := json.NewDecoder(rw.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.StepSec != 7200 {
		t.Errorf("stepSec = %d, want 7200 for range=7d", body.StepSec)
	}
	// With no metrics handle there is nothing to read, and the reason has to be
	// said: an empty chart because nobody enabled recording and an empty chart
	// because a database is down are the same picture and opposite problems.
	if body.Note == "" {
		t.Error("a long window with no long-term database came back empty and unexplained")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the raw table was queried for a window it cannot answer: %v", err)
	}
}

func TestGatewayBandwidthGetHistory_NoStore(t *testing.T) {
	h := NewGatewayBandwidthHandler(&handlers.AppState{})
	rw := httptest.NewRecorder()
	h.GetHistory(rw, httptest.NewRequest(http.MethodGet, "/api/gateway-bandwidth/history", nil))
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rw.Code)
	}
}

// rebalanceFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only SetSetting is overridden, mirroring the
// dnsFakeStore pattern in dns_settings_test.go.
type rebalanceFakeStore struct {
	store.Store
	writes []struct{ key, value string }
	setErr error
}

func (f *rebalanceFakeStore) SetSetting(key, value string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.writes = append(f.writes, struct{ key, value string }{key, value})
	return nil
}

func rebalanceReq(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/gateway-bandwidth/rebalance", bytes.NewReader([]byte(body)))
}

func TestGatewayBandwidthSetRebalanceMode_Armed(t *testing.T) {
	fs := &rebalanceFakeStore{}
	h := NewGatewayBandwidthHandler(&handlers.AppState{Store: fs, FeatureFlags: services.NewFeatureFlags(nil)})

	rw := httptest.NewRecorder()
	h.SetRebalanceMode(rw, rebalanceReq(`{"mode":"armed"}`))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rw.Code, rw.Body.String())
	}
	var out struct {
		Success bool   `json:"success"`
		Mode    string `json:"mode"`
	}
	if err := json.NewDecoder(rw.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Success || out.Mode != "armed" {
		t.Fatalf("unexpected body: %+v", out)
	}
	if len(fs.writes) != 1 || fs.writes[0].key != "warp_rebalance_mode" || fs.writes[0].value != "armed" {
		t.Fatalf("writes = %+v, want a single warp_rebalance_mode=armed write", fs.writes)
	}
}

func TestGatewayBandwidthSetRebalanceMode_RejectsUnknownMode(t *testing.T) {
	fs := &rebalanceFakeStore{}
	h := NewGatewayBandwidthHandler(&handlers.AppState{Store: fs, FeatureFlags: services.NewFeatureFlags(nil)})

	rw := httptest.NewRecorder()
	h.SetRebalanceMode(rw, rebalanceReq(`{"mode":"bogus"}`))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rw.Code, rw.Body.String())
	}
	if len(fs.writes) != 0 {
		t.Fatalf("rejected mode still wrote %+v", fs.writes)
	}
}

func TestGatewayBandwidthSetRebalanceMode_InvalidJSON(t *testing.T) {
	fs := &rebalanceFakeStore{}
	h := NewGatewayBandwidthHandler(&handlers.AppState{Store: fs, FeatureFlags: services.NewFeatureFlags(nil)})

	rw := httptest.NewRecorder()
	h.SetRebalanceMode(rw, rebalanceReq(`{not json`))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rw.Code, rw.Body.String())
	}
	if len(fs.writes) != 0 {
		t.Fatalf("malformed JSON still wrote %+v", fs.writes)
	}
}

func TestGatewayBandwidthSetRebalanceMode_StoreErrorIs500(t *testing.T) {
	fs := &rebalanceFakeStore{setErr: errors.New("db down")}
	h := NewGatewayBandwidthHandler(&handlers.AppState{Store: fs, FeatureFlags: services.NewFeatureFlags(nil)})

	rw := httptest.NewRecorder()
	h.SetRebalanceMode(rw, rebalanceReq(`{"mode":"off"}`))

	if rw.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rw.Code, rw.Body.String())
	}
}
