package auth

import (
	"dylaris-core/authz"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the session, login, 2FA, setup, account, API key, registration
// and password reset handlers read. Same field names as handlers.AppState;
// routes.go fills it (authDeps) and main builds the AuthHandler from the same
// function after every field is assigned. A nil field keeps the meaning it had
// on AppState.
type Deps struct {
	Store        store.Store
	Redis        *redis.Client
	Authz        *authz.Resolver
	FeatureFlags *services.FeatureFlags
	Events       *services.SystemEventsPublisher
	FrontendURL  string
	StoreEnabled bool
	StoreURL     string
	// AdminSecret is the RAM-only break-glass secret that gates /setup admin
	// creation; empty = feature disabled. Never persisted, never logged.
	AdminSecret  string
	SetupEnabled bool
}
