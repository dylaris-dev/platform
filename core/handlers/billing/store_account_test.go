package billing

import (
	"os"
	"strings"
	"testing"
)

// The rule this decides: whose store account the panel can read and change.
//
// These two endpoints sit in front of a SHARED-KEY channel. The key proves the
// caller is Core; it says nothing about which tenant, and the store answers for
// whichever uuid it is handed. So the uuid must come from the session and from
// nowhere else - a request body that could name it would turn "show me my
// subscription" into "show me anyone's", and "stop billing me" into "start
// billing them".
//
// Asserted by reading the source because the alternative is a live storefront:
// what has to hold is a property of the call site, not of a response.
func TestTheStoreAccountEndpointsNameNoAccountButTheSession(t *testing.T) {
	src := readHandlerSource(t, "store_account.go")

	for _, header := range []string{
		"func (h *StoreHandler) AccountSummary(w http.ResponseWriter, r *http.Request) {",
		"func (h *StoreHandler) SetBillingConsent(w http.ResponseWriter, r *http.Request) {",
	} {
		body, ok := cutFunction(src, header)
		if !ok {
			t.Errorf("%s is gone; move this assertion with it", header)
			continue
		}
		if !strings.Contains(body, `r.Context().Value("userID")`) {
			t.Errorf("%s does not take the account from the session", header)
		}
	}

	// The decoded request body must not carry a uuid at all. A field that exists
	// is a field a later edit can start trusting, and this is the one place
	// where trusting it is a whole-tenant read.
	consent, ok := cutFunction(src, "func (h *StoreHandler) SetBillingConsent(w http.ResponseWriter, r *http.Request) {")
	if !ok {
		t.Fatal("SetBillingConsent is gone")
	}
	for _, forbidden := range []string{"UUID ", "Uuid ", "uuid\"`"} {
		if strings.Contains(consent, forbidden) {
			t.Errorf("SetBillingConsent decodes a caller-supplied account id (%q)", forbidden)
		}
	}
}

// A store that is down must not look like a store account with nothing in it.
// The two read identically on screen and lead to opposite reactions - one is
// "wait", the other is "buy something".
func TestAnUnreachableStoreSaysSo(t *testing.T) {
	src := readHandlerSource(t, "store_account.go")
	body, ok := cutFunction(src, "func (h *StoreHandler) AccountSummary(w http.ResponseWriter, r *http.Request) {")
	if !ok {
		t.Fatal("AccountSummary is gone")
	}
	if !strings.Contains(body, `"reachable"`) {
		t.Error("the response cannot express that the store was not reached")
	}
	if !strings.Contains(body, "reachable\": false") && !strings.Contains(body, `"reachable": false`) {
		t.Error("the failure branch does not mark the answer unreachable")
	}
}

func readHandlerSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// cutFunction returns a function's source, from its header to the next one.
func cutFunction(src, header string) (string, bool) {
	i := strings.Index(src, header)
	if i < 0 {
		return "", false
	}
	body := src[i+len(header):]
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}
	return body, true
}
