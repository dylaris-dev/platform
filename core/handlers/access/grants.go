package access

import (
	"dylaris-core/models"
	"dylaris-core/store"
)

// RevokeUncoversInheritance reports whether removing target's direct grant on
// srv would let a wider grant flow down from srv's proxy. The resolver uses an
// inherit grant on the proxy only when there is no direct grant on the server,
// so a direct grant there can be the owner's way of narrowing what is
// inherited - and deleting it is then a widening, which members.delete must not
// be able to do, least of all to the caller's own grant. Owner and admin decide
// that as they decide inherit itself.
func RevokeUncoversInheritance(st store.Store, srv *models.Server, targetID string) bool {
	if srv == nil || srv.ProxyID == nil {
		return false
	}
	proxy, err := st.GetServerByID(*srv.ProxyID)
	if err != nil || proxy == nil || proxy.OwnerID != srv.OwnerID {
		return false
	}
	g, err := st.GetServerGrant(proxy.ID, targetID)
	// Same realm rule as the resolver: a stale row it ignores uncovers nothing.
	return err == nil && g != nil && g.Inherit && (g.OwnerUserID == "" || g.OwnerUserID == srv.OwnerID)
}
