package database

import (
	"context"
	"dylaris-core/config"
	"log"

	"github.com/redis/go-redis/v9"
)

// InitRedis dials Redis and verifies the connection before returning.
//
// Only the identity fields are set; every timeout and retry setting is left at
// the go-redis default ON PURPOSE, and the defaults were checked rather than
// assumed (v9): DialTimeout 5s, ReadTimeout and WriteTimeout 3s, MaxRetries 3
// with 8ms-512ms backoff, and a connection pool that redials lazily. The
// property that matters for resilience is that commands FAIL rather than block
// forever when Redis is unreachable, which those defaults already give.
//
// The one thing worth knowing about a Redis outage is not in this file: every
// leader-gated background loop stops, because no Core can hold the lease, so
// discovery, backups, scheduled tasks, billing, DNS and ACL reconcile all idle
// until Redis returns. Recovery is automatic - the lease is simply taken again -
// and the admin health view reports Redis down while it lasts.
//
// Pub/Sub resubscribes itself across a Redis restart and every Core subscriber
// consumes via .Channel(), so no manual reconnect logic belongs here either.
func InitRedis(cfg config.Config) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Username: cfg.RedisUser,
		Password: cfg.RedisPass,
		DB:       cfg.RedisDB,
	})

	// Ping Test
	if err := client.Ping(context.Background()).Err(); err != nil {
		log.Printf("Failed to connect to Redis at %s: %v", cfg.RedisAddr, err)
		return nil, err
	}

	log.Printf("Successfully connected to Redis at %s (DB: %d)", cfg.RedisAddr, cfg.RedisDB)
	return client, nil
}

// NewConsoleRedis is a second client to the same Redis, for the console
// streams' blocking reads only: each open console holds one connection for
// its whole life, and on the shared pool enough of them starved every other
// command of the replica. Room for many consoles; connections are dialled on
// demand and close when idle, and the handlers cap what one user can take.
func NewConsoleRedis(base *redis.Client) *redis.Client {
	opts := *base.Options()
	opts.PoolSize = 256
	return redis.NewClient(&opts)
}
