package auth

import (
	"dylaris-core/authz"
	"dylaris-core/handlers/reauth"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the account, API key, registration and password reset handlers
// read. Same field names as handlers.AppState; routes.go fills it once. A nil
// field keeps the meaning it had on AppState.
type Deps struct {
	Store        store.Store
	Redis        *redis.Client
	Authz        *authz.Resolver
	FeatureFlags *services.FeatureFlags
	FrontendURL  string
	StoreEnabled bool
	StoreURL     string
	// ReauthCodes is the second-factor check re-authentication runs. It reads
	// the replay and wrong-code limits and writes the identity audit, so it is
	// built by the caller rather than from the fields above.
	ReauthCodes reauth.CodeVerifier
}
