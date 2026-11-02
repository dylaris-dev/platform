package beam

import (
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers/access"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/services"
)

// The beam half of handlers' TestFilesAndBeamAskTheResolverForAnAdmin: an admin
// may beam to a platform machine, not to a customer's.
func TestBeamAsksTheResolverForAnAdmin(t *testing.T) {
	for _, c := range []struct {
		name    string
		node    *models.Node
		allowed bool
	}{
		{"a customer's machine", handlertest.OwnedNode(), false},
		{"a platform machine", handlertest.PlatformNode(), true},
	} {
		t.Run("beam/"+c.name, func(t *testing.T) {
			h := &BeamHandler{d: foreignDeps(c.node)}
			ok, perms := h.canBeam(handlertest.AdminRequest("GET", nil, ""), 3)
			if ok != c.allowed || perms.Write != c.allowed {
				t.Fatalf("canBeam = %v %+v, want %v", ok, perms, c.allowed)
			}
		})
	}
}

func foreignDeps(node *models.Node) Deps {
	st := &handlertest.ForeignStore{Node: node, Byon: "true"}
	flags := services.NewFeatureFlags(st)
	d := Deps{Store: st, Authz: authz.NewResolver(st)}
	d.Authz.SetForeignNode(func(nodeID int, userID string) bool {
		return access.NodeOwnedByOther(st, flags, nodeID, userID)
	})
	return d
}
