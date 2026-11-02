package users

import (
	"dylaris-core/authz"
	"dylaris-core/handlers/reauth"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the user, SSH-key and account-policy handlers read. Same field
// names as handlers.AppState; routes.go fills it once. Gateway and WarpPeers
// keep their "nil = not deployed" meaning: the interface values are copied
// as they are on AppState, so a nil stays nil.
type Deps struct {
	Store       store.Store
	Redis       *redis.Client
	Authz       *authz.Resolver
	Events      *services.SystemEventsPublisher
	CoreStorage *corestorage.Manager
	Gateway     services.GatewayProvider
	WarpPeers   services.WarpPeerDisconnector

	// The three below still live in the root handlers package (auth, totp,
	// account mail; R6/R7 move them) and reach this package as functions.
	ReauthCodes           reauth.CodeVerifier
	PasswordMinLength     func() int
	NotifyPasswordChanged func(email, username string)
}
