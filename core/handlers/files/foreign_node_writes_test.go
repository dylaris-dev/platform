package files

import (
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers/access"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/services"
)

// foreignState builds the state the way main does: the resolver asks the same
// predicate the handlers ask.
func foreignState(node *models.Node) Deps {
	st := &handlertest.ForeignStore{Node: node, Byon: "true"}
	flags := services.NewFeatureFlags(st)
	d := Deps{Store: st, Authz: authz.NewResolver(st)}
	d.Authz.SetForeignNode(func(srv *models.Server, userID string) bool {
		return access.ServerNodeOwnedByOther(st, flags, srv, userID)
	})
	return d
}

// TestFilesAndBeamAskTheResolverForAnAdmin: both answered an admin before the
// resolver was consulted, which made the resolver fence decorative.
func TestFilesAndBeamAskTheResolverForAnAdmin(t *testing.T) {
	for _, c := range []struct {
		name    string
		node    *models.Node
		allowed bool
	}{
		{"a customer's machine", handlertest.OwnedNode(), false},
		{"a platform machine", handlertest.PlatformNode(), true},
	} {
		t.Run("files/"+c.name, func(t *testing.T) {
			h := &FileHandler{d: foreignState(c.node)}
			r := handlertest.AdminRequest("GET", nil, "")
			r.URL.RawQuery = "server_uuid=srv-1"
			uuid, err := h.getServerUUID(r, "files.write")
			if (err == nil && uuid == "srv-1") != c.allowed {
				t.Fatalf("err = %v, allowed = %v", err, c.allowed)
			}
		})
	}
}
