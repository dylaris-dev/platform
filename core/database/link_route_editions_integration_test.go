package database

import (
	"bytes"
	"testing"
)

// Route-only editions: the key is ciphertext at rest, an update keeps the
// first key (the customer copied it onto their server), the owner comes from
// the route, and deleting the route takes the row with it.
// Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationLinkRouteEditions(t *testing.T) {
	db, st := integrationDB(t)
	st.SetBedrockEncryptionKey("cluster-secret-a")
	rt := linkRoute(t, st)

	if e, err := st.GetLinkRouteEdition(rt.Domain); err != nil || e != nil {
		t.Fatalf("before any write: %+v, %v", e, err)
	}
	first := bytes.Repeat([]byte{0x5A}, 16)
	if err := st.SetLinkRouteEdition(rt.Domain, true, true, first); err != nil {
		t.Fatal(err)
	}
	var enc string
	if err := db.QueryRow(`SELECT floodgate_key_enc FROM link_route_editions WHERE domain = $1`, rt.Domain).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if enc == "" || bytes.Contains([]byte(enc), first) {
		t.Fatalf("stored key is not ciphertext: %q", enc)
	}
	if err := st.SetLinkRouteEdition(rt.Domain, false, true, bytes.Repeat([]byte{0x6B}, 16)); err != nil {
		t.Fatal(err)
	}
	e, err := st.GetLinkRouteEdition(rt.Domain)
	if err != nil || e == nil || e.Java || !e.Bedrock || e.OwnerID != rt.OwnerID || !bytes.Equal(e.Key, first) {
		t.Fatalf("after update: %+v, %v (want java false, bedrock true, owner %s, the first key)", e, err, rt.OwnerID)
	}
	list, err := st.ListLinkRouteEditions()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range list {
		found = found || (l.Domain == rt.Domain && bytes.Equal(l.Key, first))
	}
	if !found {
		t.Fatalf("ListLinkRouteEditions missing the address: %+v", list)
	}
	if err := st.DeleteCoreLinkRoute(rt.Domain); err != nil {
		t.Fatal(err)
	}
	if e, err := st.GetLinkRouteEdition(rt.Domain); err != nil || e != nil {
		t.Fatalf("row survived its route: %+v, %v", e, err)
	}
}
