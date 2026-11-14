package remotegw_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/services/remotegw"
)

// keyID names a Floodgate key by length and sha256 prefix, never by its bytes.
func keyID(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("len=%d sha256=%s", len(b), hex.EncodeToString(sum[:4]))
}

// serveJSON answers every request with status and body.
func serveJSON(t *testing.T, status int, body string) *remotegw.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return remotegw.New(srv.URL, func() string { return "dyl_test_key" })
}

// An item from a provider that predates voice and Bedrock in the list reads
// nil, never "voice off" or "Java closed"; a current one reads its values.
func TestClientRouteParsesOldAndNewItems(t *testing.T) {
	old := serveJSON(t, 200, `{"success":true,"items":[{"domain":"a.example.com","target_ip":"mc-1","target_port":25565,"link_id":"link-1","proxy_protocol":false}]}`)
	rs, err := old.Routes(context.Background())
	if err != nil || len(rs) != 1 || rs[0].Voice != nil || rs[0].Bedrock != nil {
		t.Fatalf("old item = %+v, %v; want nil voice and bedrock", rs, err)
	}
	cur := serveJSON(t, 200, `{"success":true,"items":[{"domain":"a.example.com","target_ip":"mc-1","target_port":25565,"link_id":"link-1","proxy_protocol":false,`+
		`"voice":{"enabled":true,"port":30001,"target_port":24454},"bedrock":{"java":true,"bedrock":true,"has_key":true,"refusal":"no entry"}}]}`)
	rs, err = cur.Routes(context.Background())
	if err != nil || len(rs) != 1 || rs[0].Voice == nil || rs[0].Bedrock == nil {
		t.Fatalf("new item = %+v, %v", rs, err)
	}
	if *rs[0].Voice != (remotegw.RouteVoice{Enabled: true, Port: 30001, TargetPort: 24454}) ||
		*rs[0].Bedrock != (remotegw.RouteBedrock{Java: true, Bedrock: true, HasKey: true, Refusal: "no entry"}) {
		t.Fatalf("new item voice %+v bedrock %+v", *rs[0].Voice, *rs[0].Bedrock)
	}
}

func TestClientSetVoiceAndBedrock(t *testing.T) {
	ctx := context.Background()
	cl, f := newClient(t)
	m, _ := cl.MintKit(ctx, "k", "eu")
	if err := cl.PutRoute(ctx, m.LinkID, "play.example.com", "mc-12-abc123def456", 25565); err != nil {
		t.Fatal(err)
	}

	on, err := cl.SetVoice(ctx, "play.example.com", true, 0)
	if err != nil || !on.Enabled || on.Port == 0 || on.TargetPort != 24454 {
		t.Fatalf("voice on = %+v, %v", on, err)
	}
	again, err := cl.SetVoice(ctx, "play.example.com", true, 24500)
	if err != nil || again.Port != on.Port || again.TargetPort != 24500 {
		t.Fatalf("voice on again = %+v, %v; want the same port %d", again, err, on.Port)
	}
	if off, err := cl.SetVoice(ctx, "play.example.com", false, 0); err != nil || off != (remotegw.RouteVoice{}) {
		t.Fatalf("voice off = %+v, %v", off, err)
	}
	if b, err := cl.SetBedrock(ctx, "play.example.com", true, true); err != nil || b != (remotegw.RouteBedrock{Java: true, Bedrock: true, HasKey: true}) {
		t.Fatalf("bedrock on = %+v, %v", b, err)
	}
	if b, err := cl.SetBedrock(ctx, "play.example.com", true, false); err != nil || b.Bedrock || !b.HasKey {
		t.Fatalf("bedrock off = %+v, %v", b, err)
	}

	for _, c := range []struct {
		name   string
		call   func() error
		setup  func()
		status int
		code   string
	}{
		{"voice on an unknown address", func() error { _, err := cl.SetVoice(ctx, "nope.example.com", true, 0); return err }, nil, 404, "not_found"},
		{"bedrock on an unknown address", func() error { _, err := cl.SetBedrock(ctx, "nope.example.com", true, true); return err }, nil, 404, "not_found"},
		{"no edition at all", func() error { _, err := cl.SetBedrock(ctx, "play.example.com", false, false); return err }, nil, 400, "edition_invalid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var ae *remotegw.APIError
			if err := c.call(); !errors.As(err, &ae) || ae.Status != c.status || ae.Code != c.code {
				t.Fatalf("err = %v, want %d %s", err, c.status, c.code)
			}
		})
	}
	for _, code := range []string{"voice_not_allowed", "link_without_voice", "no_routes", "no_relay", "voice_pool_full", "kit_retiring"} {
		t.Run("voice refused "+code, func(t *testing.T) {
			f.SetVoiceRefusal(code)
			defer f.SetVoiceRefusal("")
			_, err := cl.SetVoice(ctx, "play.example.com", true, 0)
			var ae *remotegw.APIError
			if !errors.As(err, &ae) || ae.Status != 409 || ae.Code != code || remotegw.Retryable(err) {
				t.Fatalf("err = %v, want 409 %s, not retryable", err, code)
			}
			// Off is never refused.
			if _, err := cl.SetVoice(ctx, "play.example.com", false, 0); err != nil {
				t.Fatalf("off while on is refused: %v", err)
			}
		})
	}
	for _, code := range []string{"bedrock_disabled", "bedrock_unavailable", "kit_retiring"} {
		t.Run("bedrock refused "+code, func(t *testing.T) {
			f.SetBedrockRefusal(code)
			defer f.SetBedrockRefusal("")
			_, err := cl.SetBedrock(ctx, "play.example.com", true, true)
			var ae *remotegw.APIError
			if !errors.As(err, &ae) || ae.Status != 409 || ae.Code != code || remotegw.Retryable(err) {
				t.Fatalf("err = %v, want 409 %s, not retryable", err, code)
			}
			if _, err := cl.SetBedrock(ctx, "play.example.com", true, false); err != nil {
				t.Fatalf("Java only while adding is refused: %v", err)
			}
		})
	}
	for _, c := range []struct{ name, body string }{
		{"voice answer without voice", `{"success":true}`},
		{"bedrock answer without bedrock", `{"success":true}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl := serveJSON(t, 200, c.body)
			var err error
			if strings.HasPrefix(c.name, "voice") {
				_, err = cl.SetVoice(ctx, "a.example.com", true, 0)
			} else {
				_, err = cl.SetBedrock(ctx, "a.example.com", true, true)
			}
			if !errors.Is(err, remotegw.ErrBadAnswer) {
				t.Fatalf("err = %v, want ErrBadAnswer", err)
			}
		})
	}
}

// floodgateCases are the answers FloodgateKey must refuse; key is the value the
// answer carries, which no error may contain.
var floodgateBadAnswers = []struct {
	name string
	key  []byte
	body func(key []byte) string
}{
	{"15 bytes", []byte("0123456789abcde"), func(k []byte) string {
		return `{"success":true,"domain":"a.example.com","key":"` + base64.StdEncoding.EncodeToString(k) + `"}`
	}},
	{"17 bytes", []byte("0123456789abcdefg"), func(k []byte) string {
		return `{"success":true,"domain":"a.example.com","key":"` + base64.StdEncoding.EncodeToString(k) + `"}`
	}},
	{"not base64", []byte("0123456789abcdef"), func(k []byte) string {
		return `{"success":true,"domain":"a.example.com","key":"` + base64.StdEncoding.EncodeToString(k) + `!!"}`
	}},
	{"raw hex instead of base64", []byte("0123456789abcdef"), func(k []byte) string {
		return `{"success":true,"domain":"a.example.com","key":"` + hex.EncodeToString(k) + `"}`
	}},
	{"missing field", nil, func([]byte) string { return `{"success":true,"domain":"a.example.com"}` }},
	{"broken JSON around the key", []byte("0123456789abcdef"), func(k []byte) string {
		return `{"success":true,"key":"` + base64.StdEncoding.EncodeToString(k) + `",}`
	}},
	{"key as a number", nil, func([]byte) string { return `{"success":true,"key":12345}` }},
}

func TestClientFloodgateKey(t *testing.T) {
	ctx := context.Background()
	cl, _ := newClient(t)
	m, _ := cl.MintKit(ctx, "k", "eu")
	cl.PutRoute(ctx, m.LinkID, "play.example.com", "mc-12-abc123def456", 25565)

	if _, err := cl.FloodgateKey(ctx, "play.example.com"); !remotegw.IsCode(err, "not_found") {
		t.Fatalf("key before Bedrock: %v, want not_found", err)
	}
	if _, err := cl.SetBedrock(ctx, "play.example.com", true, true); err != nil {
		t.Fatal(err)
	}
	key, err := cl.FloodgateKey(ctx, "play.example.com")
	if err != nil || len(key) != 16 {
		t.Fatalf("key = %s, %v; want 16 bytes", keyID(key), err)
	}
	// Bedrock on again keeps the key, like the provider.
	cl.SetBedrock(ctx, "play.example.com", true, false)
	cl.SetBedrock(ctx, "play.example.com", false, true)
	if again, err := cl.FloodgateKey(ctx, "play.example.com"); err != nil || keyID(again) != keyID(key) {
		t.Fatalf("key after toggling = %s, %v; want %s", keyID(again), err, keyID(key))
	}
	if _, err := cl.FloodgateKey(ctx, ".."); !errors.Is(err, remotegw.ErrBadSegment) {
		t.Fatalf("dot segment: %v", err)
	}

	good := []byte("0123456789abcdef")
	if got, err := serveJSON(t, 200, `{"success":true,"domain":"a.example.com","key":"`+base64.StdEncoding.EncodeToString(good)+`"}`).FloodgateKey(ctx, "a.example.com"); err != nil || keyID(got) != keyID(good) {
		t.Fatalf("16 bytes = %s, %v", keyID(got), err)
	}
	for _, c := range floodgateBadAnswers {
		t.Run(c.name, func(t *testing.T) {
			got, err := serveJSON(t, 200, c.body(c.key)).FloodgateKey(ctx, "a.example.com")
			if !errors.Is(err, remotegw.ErrBadAnswer) || got != nil {
				t.Fatalf("got %s, err %v; want ErrBadAnswer", keyID(got), err)
			}
		})
	}
}

// No error FloodgateKey returns carries the key, in any encoding, or the API
// key: a refusal, a bad answer, a provider that echoes it.
func TestClientFloodgateKeyNeverInAnError(t *testing.T) {
	ctx := context.Background()
	const apiKey = "dyl_floodgate_test_key"
	type errCase struct {
		name string
		key  []byte
		err  error
	}
	var cases []errCase
	for _, c := range floodgateBadAnswers {
		_, err := serveJSON(t, 200, c.body(c.key)).FloodgateKey(ctx, "a.example.com")
		cases = append(cases, errCase{c.name, c.key, err})
	}
	leaked := []byte("fedcba9876543210")
	for _, status := range []int{400, 404, 409, 500, 502} {
		// A provider that puts the key in its refusal, as a buggy one could.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":"no %s","code":"x","key":"%s"}`, apiKey, base64.StdEncoding.EncodeToString(leaked))
		}))
		_, err := remotegw.New(srv.URL, func() string { return apiKey }).FloodgateKey(ctx, "a.example.com")
		srv.Close()
		cases = append(cases, errCase{fmt.Sprintf("refusal %d", status), leaked, err})
	}
	for _, c := range cases {
		if c.err == nil {
			t.Fatalf("%s: no error", c.name)
		}
		msg := c.err.Error()
		needles := []string{apiKey}
		if len(c.key) > 0 {
			needles = append(needles, string(c.key), base64.StdEncoding.EncodeToString(c.key),
				strings.TrimRight(base64.StdEncoding.EncodeToString(c.key), "="), hex.EncodeToString(c.key))
		}
		for _, n := range needles {
			if strings.Contains(msg, n) {
				t.Errorf("%s: the error carries a secret (%s of the key)", c.name, keyID([]byte(n)))
			}
		}
	}
}

func TestClientPlayers(t *testing.T) {
	ctx := context.Background()
	cl, f := newClient(t)
	m, _ := cl.MintKit(ctx, "k", "eu")
	for _, d := range []string{"b.example.com", "a.example.com", "idle.example.com"} {
		cl.PutRoute(ctx, m.LinkID, d, "mc-12-abc123def456", 25565)
	}
	f.SetPlayers("b.example.com", 2, 1)
	f.SetPlayers("a.example.com", 3, 0)
	f.SetPlayers("gone.example.com", 9, 9) // not a route of this account
	p, err := cl.Players(ctx)
	if err != nil || p.Java != 5 || p.Bedrock != 1 || p.AsOf.IsZero() || p.Peak == nil || len(p.Items) != 2 ||
		p.Items[0] != (remotegw.RoutePlayers{Domain: "a.example.com", Java: 3}) || p.Items[1] != (remotegw.RoutePlayers{Domain: "b.example.com", Java: 2, Bedrock: 1}) {
		t.Fatalf("players = %+v, %v", p, err)
	}
	if *p.Peak != (remotegw.PlayersPeak{}) {
		t.Fatalf("fake peak before SetPeak = %+v, want zeros", *p.Peak)
	}
	set := remotegw.PlayersPeak{Today: remotegw.PlayerPeak{Java: 7, Bedrock: 2, Total: 8}, Last30Days: remotegw.PlayerPeak{Java: 12, Bedrock: 4, Total: 15}}
	f.SetPeak(set)
	if p, err := cl.Players(ctx); err != nil || p.Peak == nil || *p.Peak != set {
		t.Fatalf("fake peak after SetPeak = %+v, %v", p.Peak, err)
	}
	withPeak := serveJSON(t, 200, `{"success":true,"asOf":"2026-11-14T10:00:00Z","total":{"java":1,"bedrock":0},"items":[],`+
		`"peak":{"today":{"java":4,"bedrock":2,"total":5},"last30Days":{"java":9,"bedrock":3,"total":11}}}`)
	if p, err := withPeak.Players(ctx); err != nil || p.Peak == nil ||
		*p.Peak != (remotegw.PlayersPeak{Today: remotegw.PlayerPeak{Java: 4, Bedrock: 2, Total: 5}, Last30Days: remotegw.PlayerPeak{Java: 9, Bedrock: 3, Total: 11}}) {
		t.Fatalf("peak = %+v, %v", p, err)
	}
}

// A provider older than the players endpoint, or older than its peak, does not
// break the caller; a real refusal still does.
func TestClientPlayersOnAnOldProvider(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		status  int
		body    string
		wantErr string // "" = no error
		empty   bool
		peak    bool
	}{
		{"router 404 without a code", 404, `{"error":"Core Error: Endpoint not found (/api/external/gateway/players)"}`, "", true, false},
		{"404 with a handler code", 404, `{"success":false,"error":"x","code":"not_found"}`, "not_found", false, false},
		{"401", 401, `{"success":false,"message":"Unauthorized"}`, "unauthorized", false, false},
		{"5xx", 503, `{"success":false,"error":"x","code":"internal"}`, "internal", false, false},
		{"no peak", 200, `{"success":true,"asOf":"2026-11-14T10:00:00Z","total":{"java":2,"bedrock":0},"items":[{"domain":"a.example.com","java":2,"bedrock":0}]}`, "", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := serveJSON(t, c.status, c.body).Players(ctx)
			if c.wantErr != "" {
				if !remotegw.IsCode(err, c.wantErr) {
					t.Fatalf("err = %v, want %s", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if c.empty && (p.Java != 0 || p.Bedrock != 0 || len(p.Items) != 0 || p.Peak != nil) {
				t.Fatalf("players = %+v, want empty", p)
			}
			if !c.empty && (p.Java != 2 || len(p.Items) != 1 || p.Peak != nil) {
				t.Fatalf("players = %+v, want 2 Java and no peak", p)
			}
		})
	}
	// The fake's own code-less 404 is the same answer.
	cl, f := newClient(t)
	f.FailNext(1, 404, "", 0)
	if p, err := cl.Players(ctx); err != nil || len(p.Items) != 0 {
		t.Fatalf("fake code-less 404 = %+v, %v", p, err)
	}
}

// The fake's two provider shapes the sweep must survive: a list that says off
// while the stored state is on, and an address re-created with a new key.
func TestFakeListSaysOffAndDropExtras(t *testing.T) {
	ctx := context.Background()
	cl, f := newClient(t)
	m, _ := cl.MintKit(ctx, "k", "eu")
	cl.PutRoute(ctx, m.LinkID, "play.example.com", "mc-12-abc123def456", 25565)
	v, _ := cl.SetVoice(ctx, "play.example.com", true, 0)
	cl.SetBedrock(ctx, "play.example.com", true, true)
	key, _ := cl.FloodgateKey(ctx, "play.example.com")

	f.SetListSaysOff(true)
	rs, err := cl.Routes(ctx)
	if err != nil || len(rs) != 1 || rs[0].Voice.Enabled || rs[0].Bedrock.Bedrock {
		t.Fatalf("list while saying off = %+v, %v", rs, err)
	}
	// The stored state is unchanged: enabling again answers the same port.
	if again, err := cl.SetVoice(ctx, "play.example.com", true, 0); err != nil || again.Port != v.Port {
		t.Fatalf("voice after a list that said off = %+v, %v; want port %d", again, err, v.Port)
	}
	f.SetListSaysOff(false)

	f.DropRouteExtras("play.example.com")
	if rs, _ := cl.Routes(ctx); rs[0].Voice.Enabled || rs[0].Bedrock.HasKey {
		t.Fatalf("after drop = voice %+v bedrock %+v", *rs[0].Voice, *rs[0].Bedrock)
	}
	if _, err := cl.FloodgateKey(ctx, "play.example.com"); !remotegw.IsCode(err, "not_found") {
		t.Fatalf("key after drop: %v", err)
	}
	cl.SetBedrock(ctx, "play.example.com", true, true)
	if fresh, err := cl.FloodgateKey(ctx, "play.example.com"); err != nil || keyID(fresh) == keyID(key) {
		t.Fatalf("re-created address kept its old key (%s), %v", keyID(fresh), err)
	}
}
