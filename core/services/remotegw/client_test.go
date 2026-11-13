package remotegw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newClient(t *testing.T) (*Client, *Fake) {
	t.Helper()
	f := NewFake(t)
	return New(f.URL(), f.Key), f
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
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != c.status || ae.Code != c.wantCode {
				t.Fatalf("err = %#v, want status %d code %s", err, c.status, c.wantCode)
			}
			if !IsCode(err, c.wantCode) || IsCode(err, "other") {
				t.Fatalf("IsCode wrong for %v", err)
			}
		})
	}
}

func TestClientRetryAfter(t *testing.T) {
	cl, f := newClient(t)
	f.FailNext(1, 429, "rate_limited", 7*time.Second)
	_, err := cl.Routes(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.RetryAfter != 7*time.Second || !Retryable(err) {
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
		{"429", &APIError{Status: 429}, true},
		{"503", &APIError{Status: 503}, true},
		{"401", &APIError{Status: 401}, false},
		{"409", &APIError{Status: 409}, false},
		{"canceled", context.Canceled, false},
		{"bad base url", ErrBaseURL, false},
		{"no key", ErrNoKey, false},
	} {
		if got := Retryable(c.err); got != c.want {
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
	if err := cl.DeleteRoute(ctx, "x.example.com"); !IsCode(err, "route_suspended") {
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
	if err := cl.PutRoute(ctx, m.LinkID, "play.example.com", "mc-1", 25570); err != nil {
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
	want := Route{Domain: "play.example.com", TargetHost: "mc-1", TargetPort: 25570, LinkID: m.LinkID}
	if err != nil || len(routes) != 1 || routes[0] != want {
		t.Fatalf("routes = %+v, %v, want %+v", routes, err, want)
	}
}

func TestClientRoutesLifecycle(t *testing.T) {
	cl, f := newClient(t)
	ctx := context.Background()
	a, _ := cl.MintKit(ctx, "a", "")
	b, _ := cl.MintKit(ctx, "b", "")
	cl.PutRoute(ctx, a.LinkID, "play.example.com", "mc-1", 0)
	if err := cl.SetProxyProtocol(ctx, "play.example.com", true); err != nil {
		t.Fatal(err)
	}
	// The move is the same upsert on another kit and keeps the proxy switch.
	if err := cl.PutRoute(ctx, b.LinkID, "play.example.com", "mc-1", 25565); err != nil {
		t.Fatal(err)
	}
	routes, _ := cl.Routes(ctx)
	if len(routes) != 1 || routes[0].LinkID != b.LinkID || !routes[0].ProxyProtocol || routes[0].TargetPort != 25565 {
		t.Fatalf("routes = %+v", routes)
	}
	if err := cl.PutRoute(ctx, "link-nope", "x.example.com", "h", 1); !IsCode(err, "link_not_found") {
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
	f := NewFake(t)

	for _, c := range []struct {
		name string
		cl   *Client
	}{
		{"error echoes the key", New(echo.URL, func() string { return key })},
		{"bad json", New(garbage.URL, func() string { return key })},
		{"unreachable", New(deadURL, func() string { return key })},
		{"rejected key", New(f.URL(), func() string { return key })},
		{"refused base url", New("http://example.com", func() string { return key })},
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
	f := NewFake(t)
	key := f.Key()
	cl := New(f.URL(), func() string { return key })
	ctx := context.Background()
	if _, err := cl.Account(ctx); err != nil {
		t.Fatal(err)
	}
	f.SetKey("dyl_rolled")
	if _, err := cl.Account(ctx); !IsCode(err, "unauthorized") {
		t.Fatalf("old key after a roll: %v", err)
	}
	key = "dyl_rolled"
	if _, err := cl.Account(ctx); err != nil {
		t.Fatalf("new key must apply without a new client: %v", err)
	}
}

func TestClientWithoutKeySendsNothing(t *testing.T) {
	f := NewFake(t)
	cl := New(f.URL(), func() string { return "" })
	if _, err := cl.Account(context.Background()); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
	if c := f.Calls(); len(c) != 0 {
		t.Fatalf("provider saw %v", c)
	}
}

func TestClientRefusesNonHTTPSUnlessLoopback(t *testing.T) {
	ctx := context.Background()
	for _, base := range []string{"http://example.com", "ftp://127.0.0.1", "example.com", "",
		"http://127.0.0.1.evil.com", "http://localhost.evil.com", "http://127.0.0.1@evil.com"} {
		if _, err := New(base, func() string { return "k" }).Account(ctx); !errors.Is(err, ErrBaseURL) {
			t.Errorf("%q: err = %v, want ErrBaseURL", base, err)
		}
	}
	for _, base := range []string{"http://127.0.0.1:1", "http://localhost:1", "http://[::1]:1"} {
		// Allowed means the failure is a dial error, not the refusal.
		if _, err := New(base, func() string { return "k" }).Account(ctx); errors.Is(err, ErrBaseURL) {
			t.Errorf("%q must be allowed", base)
		}
	}
	// https is accepted by construction; no dial needed to prove it.
	if c := New("https://panel.example.com/", func() string { return "k" }); c.baseErr != nil {
		t.Errorf("https refused: %v", c.baseErr)
	}
}

func TestClientCancelledContextIsNotRetryable(t *testing.T) {
	cl, _ := newClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cl.Account(ctx)
	if !errors.Is(err, context.Canceled) || Retryable(err) {
		t.Fatalf("err = %v, want context.Canceled and not retryable", err)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer other.Close()
	redir := httptest.NewServer(http.RedirectHandler(other.URL, http.StatusFound))
	defer redir.Close()
	_, err := New(redir.URL, func() string { return "k" }).Account(context.Background())
	var ae *APIError
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
			if !errors.Is(err, ErrBadSegment) || Retryable(err) {
				t.Errorf("%s %q: want non-retryable ErrBadSegment", name, seg)
			}
		}
	}
	if n := len(f.Calls()); n != 0 {
		t.Fatalf("%d requests went out, want none: %v", n, f.Calls())
	}
}
