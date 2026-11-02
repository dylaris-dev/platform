package storageapi

import (
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the core storage, storage connection and storage migration
// handlers read. Same field names as handlers.AppState; routes.go fills it
// once, after main.go has built StorageMigration and StorageStatus.
type Deps struct {
	Store            store.Store
	Redis            *redis.Client
	Events           *services.SystemEventsPublisher
	GRPCRegistry     *nodegrpc.Registry
	CoreStorage      *corestorage.Manager
	StorageMigration *services.StorageMigrationService
	StorageStatus    *services.StorageStatus
}
