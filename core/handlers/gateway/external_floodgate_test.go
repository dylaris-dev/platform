package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"dylaris-core/store"
)

// floodgateTestKey is distinctive so a leak is found in any encoding.
var floodgateTestKey = []byte("FGKEY-0123456789")

// keyPrint names a key in a failure message without its bytes.
func keyPrint(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("len=%d sha256=%x", len(b), sum[:4])
}

// keyLeaks reports whether s holds the test key raw, base64 or hex.
func keyLeaks(s string) bool {
	return strings.Contains(s, string(floodgateTestKey)) ||
		strings.Contains(s, base64.StdEncoding.EncodeToString(floodgateTestKey)) ||
		strings.Contains(s, base64.RawStdEncoding.EncodeToString(floodgateTestKey)) ||
		strings.Contains(s, hex.EncodeToString(floodgateTestKey))
}

// floodgateEnv is the route-only Bedrock fixture with linkDomain (owner-1)
// holding floodgateTestKey, and the external adapter over the same handler.
func floodgateEnv(t *testing.T) (*voiceSwitchEnv, *linkBedrockStore, *ExternalGatewayHandler) {
	t.Helper()
	e, ls := newLinkBedrock(t)
	ls.links[linkDomain] = store.LinkRouteEdition{Domain: linkDomain, OwnerID: "owner-1", Java: true, Bedrock: true, Key: floodgateTestKey}
	return e, ls, &ExternalGatewayHandler{gw: e.h}
}

func floodgateCall(x *ExternalGatewayHandler, owner string, admin bool, domain string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	x.GetRouteFloodgateKey(rec, extReq("GET", "/api/external/gateway/routes/"+domain+"/floodgate-key", owner, admin, map[string]string{"domain": domain}, nil))
	return rec
}

func decodeFloodgateKey(t *testing.T, rec *httptest.ResponseRecorder) []byte {
	t.Helper()
	var out struct {
		Success bool   `json:"success"`
		Domain  string `json:"domain"`
		Key     string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.Success || out.Domain != linkDomain {
		t.Fatalf("answer is not {success, domain}: status %d, err %v", rec.Code, err)
	}
	key, err := base64.StdEncoding.DecodeString(out.Key)
	if err != nil {
		t.Fatalf("key is not standard base64: %v", err)
	}
	return key
}

func TestExternalFloodgateKeyOwnerOnly(t *testing.T) {
	_, ls, x := floodgateEnv(t)
	rec := floodgateCall(x, "owner-1", false, linkDomain)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner: status %d", rec.Code)
	}
	if got := decodeFloodgateKey(t, rec); !bytes.Equal(got, floodgateTestKey) {
		t.Fatalf("owner got %s, want %s", keyPrint(got), keyPrint(floodgateTestKey))
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type %q", ct)
	}

	for _, c := range []struct {
		name  string
		owner string
		admin bool
		setup func()
	}{
		{"another account", "owner-2", false, func() {}},
		// A key carries its owner's admin flag into the context; the adapter
		// must never let it reach another account's address.
		{"an admin user's key for a foreign address", "owner-2", true, func() {}},
		// The route is the caller's, the stored key row is not.
		{"another account's key row on the caller's address", "owner-1", false, func() {
			e := ls.links[linkDomain]
			e.OwnerID = "owner-2"
			ls.links[linkDomain] = e
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.setup()
			rec := floodgateCall(x, c.owner, c.admin, linkDomain)
			if rec.Code != http.StatusNotFound || keyLeaks(rec.Body.String()) {
				t.Fatalf("status %d (want 404), body leaks key: %v", rec.Code, keyLeaks(rec.Body.String()))
			}
			var out map[string]interface{}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if out["code"] != "not_found" || out["success"] != false {
				t.Fatalf("body %v, want success false and code not_found", out)
			}
		})
	}
}

func TestExternalFloodgateKeyNoRow(t *testing.T) {
	_, ls, x := floodgateEnv(t)
	delete(ls.links, linkDomain)
	rec := floodgateCall(x, "owner-1", false, linkDomain)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusNotFound || out["code"] != "not_found" {
		t.Fatalf("no key row: %d %v, want 404 not_found", rec.Code, out)
	}
	// An address that is not the caller's at all answers the same.
	rec = floodgateCall(x, "owner-1", false, "nobody.eu.dylaris.com")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown address: %d", rec.Code)
	}
	// Another account asking about an address without a key learns nothing:
	// the route check answers before the key row is read.
	rec = floodgateCall(x, "owner-2", false, linkDomain)
	out = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusNotFound || out["error"] != "Route not found" {
		t.Fatalf("another account, no key row: %d %v, want 404 Route not found", rec.Code, out)
	}
}

func TestExternalFloodgateKeyNoStore(t *testing.T) {
	_, _, x := floodgateEnv(t)
	rec := floodgateCall(x, "owner-1", false, linkDomain)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d Cache-Control %q, want 200 no-store", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

// failingEditionStore fails the key read, for the 5xx path.
type failingEditionStore struct{ *linkBedrockStore }

func (f *failingEditionStore) GetLinkRouteEdition(string) (*store.LinkRouteEdition, error) {
	return nil, errors.New("connection reset by peer")
}

func TestExternalFloodgateKeyNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	e, ls, x := floodgateEnv(t)
	ok := floodgateCall(x, "owner-1", false, linkDomain)
	if ok.Code != http.StatusOK {
		t.Fatalf("premise: success %d", ok.Code)
	}
	if !strings.Contains(buf.String(), linkDomain) || !strings.Contains(buf.String(), "owner-1") {
		t.Fatal("a download must log the domain and the owner")
	}
	floodgateCall(x, "owner-2", false, linkDomain)

	e.h.d.Store = &failingEditionStore{ls}
	fail := floodgateCall(x, "owner-1", false, linkDomain)
	var out map[string]interface{}
	_ = json.Unmarshal(fail.Body.Bytes(), &out)
	if fail.Code != http.StatusInternalServerError || out["code"] != "internal" || out["error"] != "Internal error. Try again later." {
		t.Fatalf("store error: %d %v, want 500 internal with the fixed text", fail.Code, out)
	}
	if strings.Contains(fail.Body.String(), "connection reset") {
		t.Fatal("the store error reached the client")
	}
	if keyLeaks(buf.String()) {
		t.Fatalf("the log holds the key (%s)", keyPrint(floodgateTestKey))
	}
}

// The panel download is pinned literally: the helper extraction must not
// change a byte of it.
func TestPanelFloodgateKeyDownloadUnchanged(t *testing.T) {
	e, ls, _ := floodgateEnv(t)
	dl := linkCall(e, e.h.GetLinkRouteFloodgateKey, http.MethodGet, linkDomain, "", "owner-1", false)
	want := http.Header{
		"Content-Type":        {"application/octet-stream"},
		"Content-Disposition": {`attachment; filename="key.pem"`},
		"Content-Length":      {"16"},
		"Cache-Control":       {"no-store"},
	}
	if dl.Code != http.StatusOK || !bytes.Equal(dl.Body.Bytes(), floodgateTestKey) || fmt.Sprint(dl.Header()) != fmt.Sprint(want) {
		t.Fatalf("download: %d %s headers %v", dl.Code, keyPrint(dl.Body.Bytes()), dl.Header())
	}
	// An admin session may download any address's key, as before.
	if dl := linkCall(e, e.h.GetLinkRouteFloodgateKey, http.MethodGet, linkDomain, "", "admin-9", true); dl.Code != http.StatusOK || !bytes.Equal(dl.Body.Bytes(), floodgateTestKey) {
		t.Fatalf("admin download: %d", dl.Code)
	}

	jsonErr := func(msg string) string {
		b, _ := json.Marshal(map[string]interface{}{"message": msg, "success": false})
		return string(b) + "\n"
	}
	for _, c := range []struct {
		name   string
		user   string
		setup  func()
		status int
		body   string
	}{
		{"another owner", "owner-2", func() {}, 404, jsonErr("Route not found")},
		{"a foreign key row", "owner-1", func() {
			r := ls.links[linkDomain]
			r.OwnerID = "owner-2"
			ls.links[linkDomain] = r
		}, 404, jsonErr("Route not found")},
		{"another owner, no key yet", "owner-2", func() { delete(ls.links, linkDomain) }, 404, jsonErr("Route not found")},
		{"no key yet", "owner-1", func() { delete(ls.links, linkDomain) }, 404, jsonErr("This address has no Floodgate key yet. Give it Bedrock first.")},
		{"store error", "owner-1", func() { e.h.d.Store = &failingEditionStore{ls} }, 500, jsonErr("Failed to read the Floodgate key")},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, ls, _ = floodgateEnv(t)
			c.setup()
			dl := linkCall(e, e.h.GetLinkRouteFloodgateKey, http.MethodGet, linkDomain, "", c.user, false)
			if dl.Code != c.status || dl.Body.String() != c.body || dl.Header().Get("Content-Type") != "application/json" || len(dl.Header()) != 1 {
				t.Fatalf("%d %q headers %v, want %d %q", dl.Code, dl.Body.String(), dl.Header(), c.status, c.body)
			}
		})
	}
}
