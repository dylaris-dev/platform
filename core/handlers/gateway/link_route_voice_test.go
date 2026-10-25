package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/store"
)

// LinkVoice hands a link the voice ports of ITS routes only: another link of
// the same owner, or another owner's route on this link's token, carries a key
// this link must not hold.
func TestLinkVoiceListsOnlyThisLinksRoutes(t *testing.T) {
	h, fs, key, token := routeOnlyKit(t)
	k := bytes.Repeat([]byte{0x11}, 32)
	fs.voice = []store.LinkRouteVoice{
		{Domain: "a.eu.dylaris.com", OwnerID: "owner-1", LinkToken: token, TargetHost: "127.0.0.1", Port: 26001, TargetPort: 24454, UplinkKey: k},
		{Domain: "b.eu.dylaris.com", OwnerID: "owner-1", LinkToken: "other-link", TargetHost: "127.0.0.1", Port: 26002, TargetPort: 24454, UplinkKey: k},
		{Domain: "c.eu.dylaris.com", OwnerID: "owner-2", LinkToken: token, TargetHost: "127.0.0.1", Port: 26003, TargetPort: 24454, UplinkKey: k},
	}
	rec := callAs(h.LinkVoice, http.MethodGet, "/api/warp/link/voice", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		NowMs  int64            `json:"now_ms"`
		Routes []linkVoiceRoute `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.NowMs == 0 {
		t.Error("no clock in the answer")
	}
	if len(out.Routes) != 1 {
		t.Fatalf("want only a.eu.dylaris.com, got %+v", out.Routes)
	}
	got := out.Routes[0]
	want := linkVoiceRoute{Domain: "a.eu.dylaris.com", Subject: "owner:owner-1:voice:a.eu.dylaris.com", Port: 26001,
		Key: strings.Repeat("11", 32), Target: "127.0.0.1:24454"}
	if got != want {
		t.Fatalf("route:\n got %+v\nwant %+v", got, want)
	}
}

// An empty list is a valid answer (it is how a link learns to stop), and must
// be [] rather than null for the link's decoder.
func TestLinkVoiceEmptyIsAnEmptyList(t *testing.T) {
	h, _, key, _ := routeOnlyKit(t)
	rec := callAs(h.LinkVoice, http.MethodGet, "/api/warp/link/voice", "", key)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"routes":[]`) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestLinkPresenceAnnouncesVoice(t *testing.T) {
	got := linkPresenceValue(strings.NewReader(`{"caps":["client-addr","voice","bogus"]}`))
	if got != `{"caps":["client-addr","voice"]}` {
		t.Fatalf("presence %s", got)
	}
}

func TestSetLinkRouteVoiceRefusesABadBody(t *testing.T) {
	h := &GatewayHandler{}
	for _, body := range []string{`{}`, `{"enabled":true,"target_port":70000}`, `{"enabled":true,"target_port":-1}`, `nope`} {
		r := httptest.NewRequest(http.MethodPut, "/api/gateway/link-routes/a.eu.dylaris.com/voice", strings.NewReader(body))
		ctx := context.WithValue(r.Context(), "userID", "u")
		ctx = context.WithValue(ctx, "isAdmin", true)
		rec := httptest.NewRecorder()
		h.SetLinkRouteVoice(rec, r.WithContext(ctx))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", body, rec.Code)
		}
	}
}

// LinkTargets lists only this link's routes of this key's owner: the link
// dials nothing else, so another link's or another owner's target must not
// appear, and an empty list must be [] for the link's decoder.
func TestLinkTargetsListsOnlyThisLinksRoutes(t *testing.T) {
	h, fs, key, token := routeOnlyKit(t)
	fs.routes = []store.CoreLinkRoute{
		{Domain: "a.eu.dylaris.com", OwnerID: "owner-1", LinkToken: token, TargetHost: "127.0.0.1", TargetPort: 25565},
		{Domain: "b.eu.dylaris.com", OwnerID: "owner-1", LinkToken: "other-link", TargetHost: "127.0.0.1", TargetPort: 25566},
		{Domain: "c.eu.dylaris.com", OwnerID: "owner-2", LinkToken: token, TargetHost: "10.0.0.5", TargetPort: 25567},
	}
	rec := callAs(h.LinkTargets, http.MethodGet, "/api/warp/link/targets", "", key)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"targets":["127.0.0.1:25565"]}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	fs.routes = nil
	rec = callAs(h.LinkTargets, http.MethodGet, "/api/warp/link/targets", "", key)
	if !strings.Contains(rec.Body.String(), `"targets":[]`) {
		t.Fatalf("empty list body %s", rec.Body.String())
	}
}
