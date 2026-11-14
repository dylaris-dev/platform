package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// seedPlayersSample registers an edge and puts one stats sample in its stream,
// aged by ago.
func seedPlayersSample(t *testing.T, rdb *redis.Client, edge, raw string, ago time.Duration) {
	t.Helper()
	ctx := context.Background()
	if err := rdb.Set(ctx, "edge:registry:"+edge, `{"edge_id":"`+edge+`"}`, 0).Err(); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(time.Now().Add(-ago).UnixMilli(), 10) + "-0"
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: "dylaris:edge:" + edge + ":stats", ID: id, Values: map[string]interface{}{"data": raw}}).Err(); err != nil {
		t.Fatal(err)
	}
}

type playersAnswer struct {
	Success bool   `json:"success"`
	AsOf    string `json:"asOf"`
	Total   struct {
		Java    int `json:"java"`
		Bedrock int `json:"bedrock"`
	} `json:"total"`
	Items []struct {
		Domain  string `json:"domain"`
		Java    int    `json:"java"`
		Bedrock int    `json:"bedrock"`
	} `json:"items"`
}

func getPlayers(t *testing.T, f *extFixture, owner string) playersAnswer {
	t.Helper()
	rec := httptest.NewRecorder()
	f.h.GetPlayers(rec, extReq("GET", "/", owner, false, nil, nil))
	if rec.Code != 200 {
		t.Fatalf("players: %d %s", rec.Code, rec.Body.String())
	}
	var out playersAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("players body: %v: %s", err, rec.Body.String())
	}
	return out
}

func TestExternalPlayersOnlyTheCallersAddresses(t *testing.T) {
	f := newExtFixture(t)
	// A has players on its address and on a wildcard-free sibling nobody owns;
	// B has some of its own, one address nobody is on, and a stale sample.
	seedPlayersSample(t, f.rdb, "e1", `{"players_by_route":{"a.example.com":{"j":3,"b":1},"b.example.com":{"j":2},"orphan.example.com":{"j":9}}}`, time.Second)
	seedPlayersSample(t, f.rdb, "e2", `{"players_by_route":{"a.example.com":{"j":1}}}`, 2*time.Second)
	seedPlayersSample(t, f.rdb, "e3", `{"players_by_route":{"a.example.com":{"j":50},"b.example.com":{"b":50}}}`, time.Minute)

	a := getPlayers(t, f, extTenantA)
	if !a.Success || a.AsOf == "" {
		t.Fatalf("answer = %+v", a)
	}
	if got, want := a.Items, []struct {
		Domain  string `json:"domain"`
		Java    int    `json:"java"`
		Bedrock int    `json:"bedrock"`
	}{{"a.example.com", 4, 1}}; !reflect.DeepEqual(got, want) {
		t.Errorf("A items = %+v, want %+v (the stale sample and other accounts' addresses must not show)", got, want)
	}
	if a.Total.Java != 4 || a.Total.Bedrock != 1 {
		t.Errorf("A total = %+v", a.Total)
	}

	b := getPlayers(t, f, extTenantB)
	if len(b.Items) != 1 || b.Items[0].Domain != "b.example.com" || b.Items[0].Java != 2 || b.Items[0].Bedrock != 0 {
		t.Errorf("B items = %+v", b.Items)
	}

	c := getPlayers(t, f, extTenantC)
	if len(c.Items) != 0 || c.Total.Java != 0 || c.Total.Bedrock != 0 {
		t.Errorf("C holds nothing but sees %+v", c)
	}
	rec := httptest.NewRecorder()
	f.h.GetPlayers(rec, extReq("GET", "/", extTenantC, false, nil, nil))
	if body := rec.Body.String(); !strings.Contains(body, `"items":[]`) {
		t.Errorf("an empty list must be [], not null: %s", body)
	}
}

// ListPlayerPeaks is the SQL's answer: one owner's rows from since, newest first.
func (f *extStore) ListPlayerPeaks(owner string, since time.Time) ([]store.PlayerPeak, error) {
	var out []store.PlayerPeak
	for _, p := range f.peaks[owner] {
		if !p.Day.Before(since) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Day.After(out[b].Day) })
	return out, nil
}

func utcToday() time.Time {
	n := time.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// seedPeaks gives A rows inside and just outside the 30-day window, and B a
// higher row of its own that A must never see.
func seedPeaks(st *extStore, a, b string) {
	today := utcToday()
	st.peaks = map[string][]store.PlayerPeak{
		a: {
			{Day: today, Java: 3, Bedrock: 1, Total: 4},
			{Day: today.AddDate(0, 0, -5), Java: 9, Bedrock: 2, Total: 10},
			{Day: today.AddDate(0, 0, -29), Java: 1, Bedrock: 8, Total: 8},
			{Day: today.AddDate(0, 0, -30), Java: 50, Bedrock: 50, Total: 100},
		},
		b: {{Day: today, Java: 70, Bedrock: 70, Total: 140}},
	}
}

func TestExternalPlayersPeak(t *testing.T) {
	f := newExtFixture(t)
	seedPeaks(f.st, extTenantA, extTenantB)
	seedPlayersSample(t, f.rdb, "e1", `{"players_by_route":{"a.example.com":{"j":2}}}`, time.Second)
	type window struct{ Java, Bedrock, Total int }
	var out struct {
		playersAnswer
		Peak struct {
			Today      window `json:"today"`
			Last30Days window `json:"last30Days"`
		} `json:"peak"`
	}
	cases := []struct {
		owner         string
		today, last30 window
	}{
		{extTenantA, window{3, 1, 4}, window{9, 8, 10}},
		{extTenantB, window{70, 70, 140}, window{70, 70, 140}},
		{extTenantC, window{}, window{}},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		f.h.GetPlayers(rec, extReq("GET", "/", c.owner, false, nil, nil))
		out.Peak.Today, out.Peak.Last30Days = window{}, window{}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: %d %s", c.owner, rec.Code, rec.Body.String())
		}
		if out.Peak.Today != c.today || out.Peak.Last30Days != c.last30 {
			t.Errorf("%s: peak %+v, want today %+v last30 %+v", c.owner, out.Peak, c.today, c.last30)
		}
		if c.owner == extTenantC && !strings.Contains(rec.Body.String(), `"peak":{"last30Days":{"java":0,"bedrock":0,"total":0},"today":{"java":0,"bedrock":0,"total":0}}`) {
			t.Errorf("zeros must be written out: %s", rec.Body.String())
		}
	}
	// Task 2's fields are unchanged beside the new one.
	a := getPlayers(t, f, extTenantA)
	if !a.Success || a.AsOf == "" || a.Total.Java != 2 || len(a.Items) != 1 || a.Items[0].Domain != "a.example.com" {
		t.Errorf("task 2 fields: %+v", a)
	}
}

// A suspended address is left out of items and total, the rule the peak
// sampler (TestPlayerPeaksSumPerOwnerAcrossAddresses) and the admin view use,
// so a live total can never exceed what the same sample records as a peak.
func TestExternalPlayersLeaveSuspendedAddressesOut(t *testing.T) {
	f := newExtFixture(t)
	susp := time.Now().Add(-time.Hour)
	f.st.routes = append(f.st.routes, store.CoreLinkRoute{Domain: "a-susp.example.com", OwnerID: extTenantA, TargetHost: "10.0.0.9", TargetPort: 25565,
		SuspendedAt: &susp, SuspendedReason: "abuse report"})
	seedPlayersSample(t, f.rdb, "e1", `{"players_by_route":{"a.example.com":{"j":2,"b":1},"a-susp.example.com":{"j":5,"b":5}}}`, time.Second)
	a := getPlayers(t, f, extTenantA)
	if len(a.Items) != 1 || a.Items[0].Domain != "a.example.com" {
		t.Errorf("items %+v, want only the live address", a.Items)
	}
	if a.Total.Java != 2 || a.Total.Bedrock != 1 {
		t.Errorf("total %+v, want 2/1 (the suspended address's players left out)", a.Total)
	}
}

func TestExternalPlayersEmptyWithoutRedis(t *testing.T) {
	f := newExtFixture(t)
	f.h.gw.d.Redis = nil
	if got := getPlayers(t, f, extTenantA); !got.Success || len(got.Items) != 0 {
		t.Errorf("a Core without Redis must answer an empty list: %+v", got)
	}
}
