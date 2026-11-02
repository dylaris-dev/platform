package tabs

import (
	"net/http"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the tab, tab-proxy and spark handlers read. Same field names as
// handlers.AppState; routes.go fills it once.
type Deps struct {
	Store              store.Store
	Redis              *redis.Client
	Events             *services.SystemEventsPublisher
	FeatureFlags       *services.FeatureFlags
	Authz              *authz.Resolver
	GRPCRegistry       *nodegrpc.Registry
	DataLimiter        *services.DataLimiter
	TabMeter           *services.TabTrafficMeter
	FrontendURL        string
	TabProxyHostSuffix string
	StoreEnabled       bool
}

// Tickets is the tab-proxy ticket half of the auth handler, as functions, so
// this package does not import the root handlers package. routes.go wires it
// to AuthHandler; ParseTabProxyTicket keeps refusing anything that is not a
// tab-proxy ticket (a session token included).
type Tickets struct {
	IssueTabProxyTicket func(sessionID, username string, isAdmin bool, serverID, tabID int, readOnly bool) (string, error)
	ParseTabProxyTicket func(token string) (TabTicket, error)
	// SessionID is the id of the session behind r, "" when there is none.
	SessionID func(r *http.Request) string
}

// TabTicket is what a parsed tab-proxy ticket grants.
type TabTicket struct {
	ServerID int
	TabID    int
	ReadOnly bool
}
