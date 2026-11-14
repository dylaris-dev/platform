package remotegw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
)

func newClient(t *testing.T) (*remotegw.Client, *remotegwtest.Fake) {
	t.Helper()
	f := remotegwtest.NewFake(t)
	return remotegw.New(f.URL(), f.Key), f
}

func TestClientParsesBothErrorShapes(t *testing.T) {
	for _, c := range []struct {
		name, code string
		status     int
		wantCode   string
	}{
		{"auth layer, no code", "", 401, "unauthorized"},
		{"auth layer forbidden status", "", 403, "forbidden"},
		{"handler shape", "domain_taken", 409, "domain_taken"},
		{"status fallback", "", 502, "http_502"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl, f := newClient(t)
			f.FailNext(1, c.status, c.code, 0)
			_, err := cl.Account(context.Background())
			var ae *remotegw.APIError
			if !errors.As(err, &ae) || ae.Status != c.status || ae.Code != c.wantCode {
				t.Fatalf("err = %#v, want status %d code %s", err, c.status, c.wantCode)
			}
			if !remotegw.IsCode(err, c.wantCode) || remotegw.IsCode(err, "other") {
				t.Fatalf("IsCode wrong for %v", err)
			}
		})
	}
}

func TestClientRetryAfter(t *testing.T) {
	cl, f := newClient(t)
	f.FailNext(1, 429, "rate_limited", 7*time.Second)
	_, err := cl.Routes(context.Background())
	var ae *remotegw.APIError
	if !errors.As(err, &ae) || ae.RetryAfter != 7*time.Second || !remotegw.Retryable(err) {
		t.Fatalf("err = %#v, want RetryAfter 7s and retryable", err)
	}
}

func TestRetryable(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"network", errors.New("connection refused"), true},
		{"429", &remotegw.APIError{Status: 429}, true},
		{"503", &remotegw.APIError{Status: 503}, true},
		{"401", &remotegw.APIError{Status: 401}, false},
		{"409", &remotegw.APIError{Status: 409}, false},
		{"canceled", context.Canceled, false},
		{"bad base url", remotegw.ErrBaseURL, false},
		{"no key", remotegw.ErrNoKey, false},
	} {
		if got := remotegw.Retryable(c.err); got != c.want {
			t.Errorf("%s: Retryable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestClientDeleteRouteTreatsNotFoundAsDone(t *testing.T) {
	cl, f := newClient(t)
	ctx := context.Background()
	if err := cl.DeleteRoute(ctx, "gone.example.com"); err != nil {
		t.Fatalf("404 must be success: %v", err)
	}
	f.FailNext(1, 409, "route_suspended", 0)
	if err := cl.DeleteRoute(ctx, "x.example.com"); !remotegw.IsCode(err, "route_suspended") {
		t.Fatalf("other refusals must surface: %v", err)
	}
}

func TestClientUnwrapsItems(t *testing.T) {
	cl, f := newClient(t)
	ctx := context.Background()
	m, err := cl.MintKit(ctx, "main", "eu")
	if err != nil || m.LinkID == "" || m.LinkKey == "" {
		t.Fatalf("mint = %+v, %v", m, err)
	}
	f.SetKitOnline(m.LinkID, "client-addr", "voice")
	if err := cl.PutRoute(ctx, m.LinkID, "play.example.com", "mc-12-abc123def456", 25570); err != nil {
		t.Fatal(err)
	}
	kits, err := cl.Kits(ctx)
	if err != nil || len(kits) != 1 {
		t.Fatalf("kits = %+v, %v", kits, err)
	}
	k := kits[0]
	if k.LinkID != m.LinkID || k.Region != "eu" || k.Online == nil || !*k.Online || len(k.Caps) != 2 {
		t.Fatalf("kit = %+v", k)
	}
	routes, err := cl.Routes(ctx)
	want := remotegw.Route{Domain: "play.example.com", TargetHost: "mc-12-abc123def456", TargetPort: 25570, LinkID: m.LinkID}
	if err != nil || len(routes) != 1 || routes[0].Voice == nil || routes[0].Bedrock == nil {
		t.Fatalf("routes = %+v, %v, want one with voice and bedrock", routes, err)
	}
	got := routes[0]
	if *got.Voice != (remotegw.RouteVoice{}) || *got.Bedrock != (remotegw.RouteBedrock{Java: true}) {
		t.Fatalf("new route voice %+v bedrock %+v, want off and Java only", *got.Voice, *got.Bedrock)
	}
	got.Voice, got.Bedrock = nil, nil
	if got != want {
		t.Fatalf("routes = %+v, want %+v", got, want)
	}
}

func TestClientRoutesLifecycle(t *testing.T) {
	cl, f := newClient(t)
	ctx := context.Background()
	a, _ := cl.MintKit(ctx, "a", "")
	b, _ := cl.MintKit(ctx, "b", "")
	cl.PutRoute(ctx, a.LinkID, "play.example.com", "mc-12-abc123def456", 0)
	if err := cl.SetProxyProtocol(ctx, "play.example.com", true); err != nil {
		t.Fatal(err)
	}
	// The move is the same upsert on another kit and keeps the proxy switch.
	if err := cl.PutRoute(ctx, b.LinkID, "play.example.com", "mc-12-abc123def456", 25565); err != nil {
		t.Fatal(err)
	}
	routes, _ := cl.Routes(ctx)
	if len(routes) != 1 || routes[0].LinkID != b.LinkID || !routes[0].ProxyProtocol || routes[0].TargetPort != 25565 {
		t.Fatalf("routes = %+v", routes)
	}
	if err := cl.PutRoute(ctx, "link-nope", "x.example.com", "h", 1); !remotegw.IsCode(err, "link_not_found") {
		t.Fatalf("unknown kit: %v", err)
	}
	if err := cl.RevokeKit(ctx, b.LinkID); err != nil {
		t.Fatal(err)
	}
	if routes, _ := cl.Routes(ctx); len(routes) != 0 {
		t.Fatalf("revoking a kit must drop its routes: %+v", routes)
	}
	if got := f.Calls(); got[2] != "POST /routes play.example.com" {
		t.Fatalf("calls = %v", got)
	}
}

func TestClientReadEndpoints(t *testing.T) {
	cl, _ := newClient(t)
	ctx := context.Background()
	acct, err := cl.Account(ctx)
	if err != nil || acct.Product != "managed" || !acct.Managed || acct.Limits.MaxLinks == nil || *acct.Limits.MaxLinks != 5 ||
		acct.Limits.MaxRoutes != nil || acct.Limits.MaxCustomDomains == nil || *acct.Limits.MaxCustomDomains != 0 {
		t.Fatalf("account = %+v, %v (null must stay nil, 0 must stay 0)", acct, err)
	}
	if ok, reason, err := cl.CheckDomain(ctx, "play.example.com"); err != nil || !ok || reason != "" {
		t.Fatalf("check = %v %q %v", ok, reason, err)
	}
	if ok, reason, _ := cl.CheckDomain(ctx, ""); ok || reason != "invalid" {
		t.Fatalf("empty check = %v %q", ok, reason)
	}
	reg, err := cl.Regions(ctx)
	if err != nil || len(reg.Items) != 1 || reg.Items[0].Region != "eu" || reg.CNAMETarget == "" || len(reg.HosterDomains) != 1 {
		t.Fatalf("regions = %+v, %v", reg, err)
	}
	u, err := cl.Usage(ctx, "2026-11")
	if err != nil || u.EdgeBytes != 1000 || u.RelayBytes != 20 || u.Final || u.Pool == nil || u.Pool.IncludedGB != 5000 || u.Pool.MaxPurchaseGB != nil {
		t.Fatalf("usage = %+v, %v", u, err)
	}
}

func TestClientNeverPutsTheKeyInAnError(t *testing.T) {
	const key = "dyl_super_secret_key"
	ctx := context.Background()

	// A provider that echoes the key in its message, as a hostile or buggy one could.
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"bad ` + key + `"}`))
	}))
	defer echo.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not json")) }))
	defer garbage.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	f := remotegwtest.NewFake(t)

	for _, c := range []struct {
		name string
		cl   *remotegw.Client
	}{
		{"error echoes the key", remotegw.New(echo.URL, func() string { return key })},
		{"bad json", remotegw.New(garbage.URL, func() string { return key })},
		{"unreachable", remotegw.New(deadURL, func() string { return key })},
		{"rejected key", remotegw.New(f.URL(), func() string { return key })},
		{"refused base url", remotegw.New("http://example.com", func() string { return key })},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.cl.Account(ctx)
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), key) {
				t.Fatalf("error carries the key: %v", err)
			}
		})
	}
}

func TestClientKeyIsReadPerRequest(t *testing.T) {
	f := remotegwtest.NewFake(t)
	key := f.Key()
	cl := remotegw.New(f.URL(), func() string { return key })
	ctx := context.Background()
	if _, err := cl.Account(ctx); err != nil {
		t.Fatal(err)
	}
	f.SetKey("dyl_rolled")
	if _, err := cl.Account(ctx); !remotegw.IsCode(err, "unauthorized") {
		t.Fatalf("old key after a roll: %v", err)
	}
	key = "dyl_rolled"
	if _, err := cl.Account(ctx); err != nil {
		t.Fatalf("new key must apply without a new client: %v", err)
	}
}

func TestClientWithoutKeySendsNothing(t *testing.T) {
	f := remotegwtest.NewFake(t)
	cl := remotegw.New(f.URL(), func() string { return "" })
	if _, err := cl.Account(context.Background()); !errors.Is(err, remotegw.ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
	if c := f.Calls(); len(c) != 0 {
		t.Fatalf("provider saw %v", c)
	}
}

func TestClientCancelledContextIsNotRetryable(t *testing.T) {
	cl, _ := newClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cl.Account(ctx)
	if !errors.Is(err, context.Canceled) || remotegw.Retryable(err) {
		t.Fatalf("err = %v, want context.Canceled and not retryable", err)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer other.Close()
	redir := httptest.NewServer(http.RedirectHandler(other.URL, http.StatusFound))
	defer redir.Close()
	_, err := remotegw.New(redir.URL, func() string { return "k" }).Account(context.Background())
	var ae *remotegw.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusFound || leaked {
		t.Fatalf("err = %v, leaked = %v, want APIError 302 and no second request", err, leaked)
	}
}

func TestClientRefusesDotPathSegments(t *testing.T) {
	cl, f := newClient(t)
	ctx := context.Background()
	for _, seg := range []string{".", "..", ""} {
		for name, err := range map[string]error{
			"revoke": cl.RevokeKit(ctx, seg),
			"delete": cl.DeleteRoute(ctx, seg),
			"proxy":  cl.SetProxyProtocol(ctx, seg, true),
		} {
			if !errors.Is(err, remotegw.ErrBadSegment) || remotegw.Retryable(err) {
				t.Errorf("%s %q: want non-retryable ErrBadSegment", name, seg)
			}
		}
	}
	if n := len(f.Calls()); n != 0 {
		t.Fatalf("%d requests went out, want none: %v", n, f.Calls())
	}
}

// RetireKit returns the provider's deadline, the kit list carries it, and the
// provider's refusals reach the caller with their codes.
func TestClientRetireKit(t *testing.T) {
	ctx := context.Background()
	cl, f := newClient(t)
	f.SetLinkLimit(1)
	m, err := cl.MintKit(ctx, "old", "eu")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	at, err := cl.RetireKit(ctx, m.LinkID)
	if err != nil {
		t.Fatal(err)
	}
	if d := at.Sub(before); d < 8*time.Hour-2*time.Second || d > 8*time.Hour+2*time.Second {
		t.Fatalf("retire_at %v is %v away, want 8h", at, d)
	}
	kits, err := cl.Kits(ctx)
	if err != nil || len(kits) != 1 || kits[0].RetireAt == nil || !kits[0].RetireAt.Equal(at) {
		t.Fatalf("kits %+v, %v: want retire_at %v", kits, err, at)
	}
	m2, err := cl.MintKit(ctx, "new", "eu")
	if err != nil {
		t.Fatalf("mint beside a retiring kit at a limit of 1: %v", err)
	}
	if kits, _ := cl.Kits(ctx); len(kits) != 2 || kits[1].RetireAt != nil {
		t.Fatalf("a kit that is not retiring carries retire_at: %+v", kits)
	}

	for _, c := range []struct {
		name, link, code string
		setup            func()
	}{
		{"already retiring", m.LinkID, "kit_already_retiring", nil},
		{"every slot in use", m2.LinkID, "retire_limit", nil},
		{"budget used up", m2.LinkID, "retire_budget", func() { f.SetLinkLimit(2); f.SetRetireBudget(1) }},
		{"unknown kit", "link-nope", "link_not_found", nil},
		{"no host limit", m2.LinkID, "retire_not_needed", func() { f.SetLinkLimit(0) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.setup != nil {
				c.setup()
			}
			at, err := cl.RetireKit(ctx, c.link)
			if !remotegw.IsCode(err, c.code) || !at.IsZero() || remotegw.Retryable(err) {
				t.Fatalf("got %v, %v; want %s, not retryable", at, err, c.code)
			}
		})
	}
	if _, err := cl.RetireKit(ctx, ".."); !errors.Is(err, remotegw.ErrBadSegment) {
		t.Fatalf("dot segment: %v", err)
	}
}
