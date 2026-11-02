package beam

import (
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers"
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
			h := &BeamHandler{state: foreignState(c.node)}
			ok, perms := h.canBeam(handlertest.AdminRequest("GET", nil, ""), 3)
			if ok != c.allowed || perms.Write != c.allowed {
				t.Fatalf("canBeam = %v %+v, want %v", ok, perms, c.allowed)
			}
		})
	}
}

func foreignState(node *models.Node) *handlers.AppState {
	st := &handlertest.ForeignStore{Node: node, Byon: "true"}
	s := &handlers.AppState{Store: st, Authz: authz.NewResolver(st), FeatureFlags: services.NewFeatureFlags(st)}
	s.Authz.SetForeignNode(s.NodeOwnedByOther)
	return s
}
