package handlers

import (
	"strings"
	"testing"

	"dylaris-core/models"
)

// The .mrpack path is a credential, not an address.
//
// The Node downloads it over /solder/mirror/, which is anonymous - a Technic
// launcher cannot present one, so the whole route is. The layout is only safe
// while the path cannot be reconstructed from what the platform publishes, and
// the public Solder API publishes the pack owner's id in every mods[].url. A
// path built from that id left just the internal slug between an anonymous
// caller and the full .mrpack of every other pack on the account.
func TestMrpackStorageKeyRevealsNoPublishedComponent(t *testing.T) {
	h := &PacksHandler{state: &AppState{ClusterSecret: "cluster-secret-under-test"}}
	pack := &models.Pack{OwnerID: "aaaaaaaa-1111-4111-8111-111111111111", InternalSlug: "skyfactory"}
	build := &models.PackBuild{VersionString: "1.2.3"}

	key := h.mrpackStorageKey(pack, build)

	if !strings.HasPrefix(key, "modpacks/") || !strings.HasSuffix(key, "/pack.mrpack") {
		t.Fatalf("key %q is outside the prefix SolderMirror serves", key)
	}
	// The owner id is the one component an anonymous caller already holds.
	for _, leak := range []string{pack.OwnerID, pack.InternalSlug, build.VersionString} {
		if strings.Contains(key, leak) {
			t.Errorf("key %q contains %q, which makes the path derivable", key, leak)
		}
	}
}

// Stable per (owner, pack, build): a draft install re-renders on every request
// and must overwrite its object rather than leave a new one behind each time.
func TestMrpackStorageKeyIsStableAndDistinct(t *testing.T) {
	h := &PacksHandler{state: &AppState{ClusterSecret: "cluster-secret-under-test"}}
	pack := &models.Pack{OwnerID: "aaaaaaaa-1111-4111-8111-111111111111", InternalSlug: "skyfactory"}
	build := &models.PackBuild{VersionString: "1.2.3"}

	first := h.mrpackStorageKey(pack, build)
	if second := h.mrpackStorageKey(pack, build); first != second {
		t.Fatalf("not stable: %q vs %q", first, second)
	}

	// Two accounts may hold the same slug since uniqueness moved to
	// (owner_id, solder_slug), so the owner has to reach the key.
	other := &models.Pack{OwnerID: "bbbbbbbb-2222-4222-8222-222222222222", InternalSlug: "skyfactory"}
	if h.mrpackStorageKey(other, build) == first {
		t.Error("two owners with the same slug collide on one object")
	}
	if h.mrpackStorageKey(pack, &models.PackBuild{VersionString: "1.2.4"}) == first {
		t.Error("two builds of one pack collide on one object")
	}
	// The separator earns its place: without it ("sky", "factory1.2.3") and
	// ("skyfactory", "1.2.3") would hash to the same object.
	seam := &models.Pack{OwnerID: pack.OwnerID, InternalSlug: "sky"}
	if h.mrpackStorageKey(seam, &models.PackBuild{VersionString: "factory1.2.3"}) == first {
		t.Error("component boundaries are not encoded; concatenation collides")
	}
}

// A deployment's paths must not be reproducible from another deployment's.
func TestMrpackStorageKeyIsPerDeployment(t *testing.T) {
	pack := &models.Pack{OwnerID: "aaaaaaaa-1111-4111-8111-111111111111", InternalSlug: "skyfactory"}
	build := &models.PackBuild{VersionString: "1.2.3"}

	a := (&PacksHandler{state: &AppState{ClusterSecret: "secret-a"}}).mrpackStorageKey(pack, build)
	b := (&PacksHandler{state: &AppState{ClusterSecret: "secret-b"}}).mrpackStorageKey(pack, build)
	if a == b {
		t.Error("the path does not depend on CLUSTER_SECRET, so anyone can compute it")
	}
}
