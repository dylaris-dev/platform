package database

import (
	"testing"

	"dylaris-core/store"
)

func TestIntegrationListCoreLinkRoutesByOwner(t *testing.T) {
	_, st := integrationDB(t)
	a, b := uniqueName("own_a_"), uniqueName("own_b_")
	rows := []store.CoreLinkRoute{
		{Domain: "z." + a + ".example.test", OwnerID: a, LinkToken: "t", TargetHost: "127.0.0.1", TargetPort: 25565},
		{Domain: "y." + a + ".example.test", OwnerID: a, LinkToken: "t", TargetHost: "127.0.0.1", TargetPort: 25566},
		{Domain: "x." + b + ".example.test", OwnerID: b, LinkToken: "t", TargetHost: "127.0.0.1", TargetPort: 25567},
	}
	for _, r := range rows {
		r := r
		if err := st.UpsertCoreLinkRoute(r); err != nil {
			t.Fatalf("UpsertCoreLinkRoute: %v", err)
		}
		t.Cleanup(func() { st.DeleteCoreLinkRoute(r.Domain) })
	}
	got, err := st.ListCoreLinkRoutesByOwner(a)
	if err != nil {
		t.Fatalf("ListCoreLinkRoutesByOwner: %v", err)
	}
	if len(got) != 2 || got[0].Domain != rows[1].Domain || got[1].Domain != rows[0].Domain {
		t.Fatalf("owner a rows = %+v, want the two of a ordered by domain", got)
	}
	if none, err := st.ListCoreLinkRoutesByOwner(uniqueName("own_none_")); err != nil || len(none) != 0 {
		t.Fatalf("unknown owner = %v, %v, want empty", none, err)
	}
}
