package services

import (
	"context"
	"errors"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// managedBedrockFake answers the managed-plan lookup per owner and counts it.
type managedBedrockFake struct {
	*bedrockFakeStore
	managed map[string]bool
	err     error
	lookups int
}

func (f *managedBedrockFake) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	f.lookups++
	if f.err != nil {
		return nil, f.err
	}
	if region == TrafficRegionAny && kind == TrafficKindEdge && f.managed[scope[len("user:"):]] {
		return &models.TrafficLimit{Region: region, Kind: kind}, nil
	}
	return nil, nil
}

// One server route and four route-only addresses: two of a managed owner
// (one Java+Bedrock, one Bedrock only), one of a plain owner, one Java only.
func newManagedBedrockFake() *managedBedrockFake {
	fs := newBedrockFake()
	fs.eds = []store.RouteEdition{{Domain: "srv.example.com", ServerID: 7, ServerUUID: "uuid-7", Java: true, Bedrock: true}}
	key := []byte("0123456789abcdef")
	fs.links = []store.LinkRouteEdition{
		{Domain: "m1.example.com", OwnerID: "managed", Java: true, Bedrock: true, Key: key},
		{Domain: "m2.example.com", OwnerID: "managed", Java: false, Bedrock: true, Key: key},
		{Domain: "plain.example.com", OwnerID: "plain", Java: true, Bedrock: true, Key: key},
		{Domain: "java.example.com", OwnerID: "javaonly", Java: true, Bedrock: false, Key: key},
	}
	return &managedBedrockFake{bedrockFakeStore: fs, managed: map[string]bool{"managed": true}}
}

func TestPublishEditionsManagedOwnerActiveUnderOff(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newManagedBedrockFake()
	if err := NewBedrockService(fs, rdb).PublishEditions(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	for domain, want := range map[string]string{
		"m1.example.com":    "both",
		"m2.example.com":    "bedrock",
		"plain.example.com": "", // not managed: paused as before
		"java.example.com":  "",
		"srv.example.com":   "", // server routes follow the switch alone
	} {
		if got := mr.HGet(RouteEditionsKey, domain); got != want {
			t.Errorf("%s = %q, want %q", domain, got, want)
		}
	}
	// Once per owner that has a Bedrock row: managed and plain, not javaonly.
	if fs.lookups != 2 {
		t.Errorf("%d managed lookups, want 2 (one per owner with Bedrock)", fs.lookups)
	}

	// The plan ended: the next publish pauses it like everyone else.
	fs.managed = map[string]bool{}
	if err := NewBedrockService(fs, rdb).PublishEditions(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet(RouteEditionsKey, "m1.example.com"); got != "" {
		t.Errorf("after the plan ended m1 = %q, want Java only", got)
	}
	if got := mr.HGet(RouteEditionsKey, "m2.example.com"); got != "none" {
		t.Errorf("after the plan ended m2 = %q, want none", got)
	}
}

// admin and all are active: same hash as before, and no lookup at all.
func TestPublishEditionsActiveAsksNoPlan(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newManagedBedrockFake()
	if err := NewBedrockService(fs, rdb).PublishEditions(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for domain, want := range map[string]string{
		"m1.example.com": "both", "m2.example.com": "bedrock", "plain.example.com": "both", "java.example.com": "", "srv.example.com": "both",
	} {
		if got := mr.HGet(RouteEditionsKey, domain); got != want {
			t.Errorf("%s = %q, want %q", domain, got, want)
		}
	}
	if fs.lookups != 0 {
		t.Errorf("%d managed lookups while active, want 0", fs.lookups)
	}
}

// A failed lookup aborts before Redis is touched: the published hash and the
// keys stay exactly as they were, rather than every managed customer's
// Bedrock being paused for a tick.
func TestPublishEditionsManagedLookupErrorTouchesNothing(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	mr.HSet(RouteEditionsKey, "m1.example.com", "both")
	mr.HSet(RouteEditionsKey, "stale.example.com", "both")
	before := mr.Dump()
	fs := newManagedBedrockFake()
	fs.err = errors.New("connection reset by peer")
	if err := NewBedrockService(fs, rdb).PublishEditions(context.Background(), false); err == nil {
		t.Fatal("a failed plan lookup must fail the publish")
	}
	if after := mr.Dump(); after != before {
		t.Fatalf("Redis changed on a failed lookup:\nbefore %s\nafter  %s", before, after)
	}
}
