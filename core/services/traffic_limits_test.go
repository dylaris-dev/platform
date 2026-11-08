package services

import (
	"errors"
	"strconv"
	"testing"

	"dylaris-core/models"
)

func gbPtr(n int64) *int64 { return &n }

type fakeLimitReader struct {
	rows map[string]*models.TrafficLimit // "scope|region|kind" -> row
	err  error
	asks []string
}

func (f *fakeLimitReader) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	f.asks = append(f.asks, scope)
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[scope+"|"+region+"|"+kind], nil
}

func TestResolveTrafficLimit_ScopeOrder(t *testing.T) {
	tests := []struct {
		name        string
		region      string
		rows        map[string]*models.TrafficLimit
		wantScope   string
		wantIncl    *int64
		wantPurch   *int64
		wantAskedUp int // how many scopes should have been consulted
	}{
		{
			name:        "nothing configured anywhere is no limit",
			region:      "eu-central",
			rows:        nil,
			wantScope:   "",
			wantIncl:    nil,
			wantPurch:   nil,
			wantAskedUp: 2,
		},
		{
			name:   "the tenant default answers when the user has no row",
			region: "eu-central",
			rows: map[string]*models.TrafficLimit{
				"user_default|eu-central|edge": {IncludedGB: gbPtr(1000), MaxPurchaseGB: gbPtr(5000)},
			},
			wantScope:   "user_default",
			wantIncl:    gbPtr(1000),
			wantPurch:   gbPtr(5000),
			wantAskedUp: 2,
		},
		{
			name:   "the user's own row beats the default and stops the walk",
			region: "eu-central",
			rows: map[string]*models.TrafficLimit{
				"user:u1|eu-central|edge":      {IncludedGB: gbPtr(9000), MaxPurchaseGB: gbPtr(1)},
				"user_default|eu-central|edge": {IncludedGB: gbPtr(2000)},
			},
			wantScope:   "user:u1",
			wantIncl:    gbPtr(9000),
			wantPurch:   gbPtr(1),
			wantAskedUp: 1,
		},
		{
			// The distinction the whole convention exists for. A row that
			// exists has ANSWERED, and its answer may be NULL - "set, and no
			// limit". It must not fall through to a scope that would have said
			// something, or an operator lifting one tenant's cap would silently
			// hand them the default one instead.
			name:   "a row with NULL values answers and does not fall through",
			region: "eu-central",
			rows: map[string]*models.TrafficLimit{
				"user:u1|eu-central|edge":      {IncludedGB: nil, MaxPurchaseGB: nil},
				"user_default|eu-central|edge": {IncludedGB: gbPtr(1000), MaxPurchaseGB: gbPtr(0)},
			},
			wantScope:   "user:u1",
			wantIncl:    nil,
			wantPurch:   nil,
			wantAskedUp: 1,
		},
		{
			// Zero is a real answer, not an absence. This is the case that
			// shipped as a defect four times on this platform before limits
			// became pointers.
			name:   "zero is a cap, not a missing value",
			region: "ap-southeast",
			rows: map[string]*models.TrafficLimit{
				"user_default|ap-southeast|edge": {IncludedGB: gbPtr(0), MaxPurchaseGB: gbPtr(0)},
			},
			wantScope:   "user_default",
			wantIncl:    gbPtr(0),
			wantPurch:   gbPtr(0),
			wantAskedUp: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLimitReader{rows: tt.rows}
			got, err := ResolveTrafficLimit(f, "u1", tt.region, "edge")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Scope != tt.wantScope {
				t.Errorf("scope = %q, want %q", got.Scope, tt.wantScope)
			}
			assertPtr(t, "includedGB", got.IncludedGB, tt.wantIncl)
			assertPtr(t, "maxPurchaseGB", got.MaxPurchaseGB, tt.wantPurch)
			// Distinct scopes: the user scope is asked twice (exact region, then "*").
			distinct := map[string]bool{}
			for _, s := range f.asks {
				distinct[s] = true
			}
			if len(distinct) != tt.wantAskedUp {
				t.Errorf("consulted %v, want %d scope(s)", f.asks, tt.wantAskedUp)
			}
		})
	}
}

// The retired "global" scope must not answer any more.
//
// It is asserted rather than assumed because a leftover row is exactly what the
// migration handles, and a resolver that still consulted the scope would keep
// serving a number the operator can no longer see or edit on any screen.
func TestResolveTrafficLimit_RetiredGlobalScopeIsNotConsulted(t *testing.T) {
	f := &fakeLimitReader{rows: map[string]*models.TrafficLimit{
		"global|eu-central|edge": {IncludedGB: gbPtr(1000)},
	}}
	got, err := ResolveTrafficLimit(f, "u1", "eu-central", "edge")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Scope != "" {
		t.Fatalf("the retired global scope answered: %+v", got)
	}
	for _, asked := range f.asks {
		if asked == "global" {
			t.Fatalf("global was still consulted: %v", f.asks)
		}
	}
}

// A (region, kind) is asked as a pair: a limit set for player traffic must not
// answer for file transfers, which are carried by a different component at a
// different price.
func TestResolveTrafficLimit_KindAndRegionAreThePair(t *testing.T) {
	f := &fakeLimitReader{rows: map[string]*models.TrafficLimit{
		"user_default|eu-central|edge": {IncludedGB: gbPtr(1000)},
	}}
	if got, _ := ResolveTrafficLimit(f, "u1", "eu-central", "relay"); got.Scope != "" {
		t.Errorf("an edge limit answered for relay: %+v", got)
	}
	if got, _ := ResolveTrafficLimit(f, "u1", "ap-southeast", "edge"); got.Scope != "" {
		t.Errorf("a eu-central limit answered for ap-southeast: %+v", got)
	}
}

// A user's "*" edge row covers every region without an exact user row, beats
// the operator default, and loses to an exact user row. The default scope's "*"
// edge row stays dead, as it was before.
func TestResolveTrafficLimit_UserWildcardRegion(t *testing.T) {
	tests := []struct {
		name      string
		rows      map[string]*models.TrafficLimit
		region    string
		wantScope string
		wantIncl  *int64
	}{
		{"wildcard answers any region", map[string]*models.TrafficLimit{
			"user:u1|*|edge": {IncludedGB: gbPtr(5000), MaxPurchaseGB: gbPtr(10000)},
		}, "us-east", "user:u1", gbPtr(5000)},
		{"wildcard beats the default", map[string]*models.TrafficLimit{
			"user:u1|*|edge":               {IncludedGB: gbPtr(5000)},
			"user_default|eu-central|edge": {IncludedGB: gbPtr(1000)},
		}, "eu-central", "user:u1", gbPtr(5000)},
		{"exact user row beats the wildcard", map[string]*models.TrafficLimit{
			"user:u1|*|edge":          {IncludedGB: gbPtr(5000)},
			"user:u1|eu-central|edge": {IncludedGB: gbPtr(7000)},
		}, "eu-central", "user:u1", gbPtr(7000)},
		{"default wildcard stays dead", map[string]*models.TrafficLimit{
			"user_default|*|edge": {IncludedGB: gbPtr(1000)},
		}, "eu-central", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveTrafficLimit(&fakeLimitReader{rows: tt.rows}, "u1", tt.region, "edge")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Scope != tt.wantScope {
				t.Errorf("scope = %q, want %q", got.Scope, tt.wantScope)
			}
			assertPtr(t, "includedGB", got.IncludedGB, tt.wantIncl)
		})
	}
}

// File transfers have ONE allowance, wherever the bytes were recorded.
//
// Every beam relay sits in eu-central, so a per-region cap would be a row per
// region answering a question with one possible answer - and, worse, a tenant
// whose transfers were attributed to two regions would get the whole allowance
// in each of them.
func TestResolveTrafficLimit_NonRegionalKindsShareOneRow(t *testing.T) {
	f := &fakeLimitReader{rows: map[string]*models.TrafficLimit{
		"user_default|" + TrafficRegionAny + "|relay": {IncludedGB: gbPtr(1000)},
	}}
	for _, region := range []string{"eu-central", "us-east", "ap-southeast", ""} {
		got, err := ResolveTrafficLimit(f, "u1", region, "relay")
		if err != nil {
			t.Fatalf("region %q: %v", region, err)
		}
		if got.Scope != "user_default" || got.IncludedGB == nil || *got.IncludedGB != 1000 {
			t.Errorf("region %q: the one relay row should have answered, got %+v", region, got)
		}
	}
	// And a relay row must NOT be found under a concrete region, or an operator
	// could set a number on a screen that nothing reads.
	stray := &fakeLimitReader{rows: map[string]*models.TrafficLimit{
		"user_default|eu-central|relay": {IncludedGB: gbPtr(1000)},
	}}
	if got, _ := ResolveTrafficLimit(stray, "u1", "eu-central", "relay"); got.Scope != "" {
		t.Errorf("a relay row stored under a concrete region answered: %+v", got)
	}
}

func TestTrafficLimitRegion(t *testing.T) {
	tests := []struct {
		region, kind, want string
	}{
		{"eu-central", TrafficKindEdge, "eu-central"},
		{"ap-southeast", TrafficKindEdge, "ap-southeast"},
		{"eu-central", TrafficKindRelay, TrafficRegionAny},
		{"us-east", TrafficKindRelay, TrafficRegionAny},
		{"eu-central", "warp", TrafficRegionAny},
	}
	for _, tt := range tests {
		if got := TrafficLimitRegion(tt.region, tt.kind); got != tt.want {
			t.Errorf("TrafficLimitRegion(%q, %q) = %q, want %q", tt.region, tt.kind, got, tt.want)
		}
	}
}

func TestResolveTrafficLimit_StoreErrorSurfaces(t *testing.T) {
	f := &fakeLimitReader{err: errors.New("db down")}
	if _, err := ResolveTrafficLimit(f, "u1", "eu-central", "edge"); err == nil {
		t.Fatal("a store failure must not read as 'no limit configured'")
	}
}

func assertPtr(t *testing.T, what string, got, want *int64) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s = %s, want %s", what, ptrStr(got), ptrStr(want))
	case *got != *want:
		t.Errorf("%s = %d, want %d", what, *got, *want)
	}
}

func ptrStr(p *int64) string {
	if p == nil {
		return "nil"
	}
	return "ptr(" + strconv.FormatInt(*p, 10) + ")"
}

// There is no backup traffic kind, and that absence is deliberate.
//
// Backups go from the node straight to object storage and R2 charges nothing for
// ingress, so on a BYON node the bytes are the customer's own bandwidth and cost
// us nothing either way. One was added and removed again: a pool that caps and
// bills a resource we do not pay for is a control somebody eventually reads as
// meaningful. What backups cost is STORAGE, counted as r2_quota_gb.
func TestBackupIsNotATrafficKind(t *testing.T) {
	if RegionalKind("backup") {
		t.Error("backup must not be a regional traffic kind")
	}
	// And it must not resolve to the shared non-regional row either, which would
	// silently put it in the file-transfer pool.
	f := &fakeLimitReader{rows: map[string]*models.TrafficLimit{
		"user_default|" + TrafficRegionAny + "|relay": {IncludedGB: gbPtr(1000)},
	}}
	if got, _ := ResolveTrafficLimit(f, "u1", "eu-central", "backup"); got.Scope != "" {
		t.Errorf("a backup lookup found a limit: %+v", got)
	}
}
