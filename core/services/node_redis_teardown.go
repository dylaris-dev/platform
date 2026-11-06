package services

import (
	"context"
	"log"
	"strings"

	"dylaris-core/services/redisacl"

	"github.com/redis/go-redis/v9"
)

// NodeRedisKeys are the keys a node OWNS, keyed by its token. None of them
// expire and nothing else removes them, so every node whose row goes without
// this leaves a few behind for good.
//
// One list, because there are two callers now - an operator deleting a node,
// and an account teardown deleting the nodes a departing tenant brought - and a
// second copy is how one of them ends up missing a key nobody notices.
func NodeRedisKeys(token string) []string {
	return []string{
		"dylaris:node:" + token + ":netpolicy",
		NetPolicyTenancyKey(token),
		"dylaris:node:" + token + ":storage_placement",
		"dylaris:node:" + token + ":cpu",
		"dylaris:node:" + token + ":cpu:sig",
		"dylaris:node:" + token + ":cpuload",
		"dylaris:node:" + token + ":cmds",
		"dylaris:node:" + token + ":cmds:dead",
		"dylaris:node:" + token + ":ports",
		// The node's stats streams, with the stats consumer group and its
		// pending list: MAXLEN-capped but never expiring.
		"dylaris:node:" + token + ":stats:batch",
		"dylaris:node:" + token + ":system",
		NodeBackupPolicyKey(token),
		NodeBackupEnvKey(token),
	}
}

// RemoveNodeRedisState revokes a node's scoped Redis users and drops the keys
// it owns.
//
// The ACL half is the one that matters. A node caches its secret in
// dylaris_data on purpose - it survives a container recreate - so removing the
// row alone does NOT revoke anything: the machine keeps authenticating with the
// credential it already holds. ACL DELUSER disconnects live clients rather than
// waiting for a reconnect that a removed node has no reason to make.
func RemoveNodeRedisState(ctx context.Context, rdb *redis.Client, prov *redisacl.Provisioner, token string) {
	if rdb == nil || token == "" {
		return
	}
	if prov != nil {
		prov.RemoveNodeACL(ctx, token)
	}
	if err := rdb.Del(ctx, NodeRedisKeys(token)...).Err(); err != nil {
		log.Printf("node teardown: leftover Redis keys for node token %.8s... could not be removed: %v", token, err)
	}
	removeNodePortKeys(ctx, rdb, token)
}

// removeNodePortKeys drops the node's per-server port keys
// (dylaris:node:<token>:port:<uuid>) and its per-server storage and disk keys
// (node:<token>:server:<uuid>:storage, :disk_limit_mb, :disk_grace, ...).
// Only the node itself removes one, so a node deleted while offline left them
// all for good. A token with a glob character would match other nodes' keys,
// so it is not scanned.
func removeNodePortKeys(ctx context.Context, rdb *redis.Client, token string) {
	if strings.ContainsAny(token, "*?[]\\") {
		return
	}
	for _, pattern := range []string{"dylaris:node:" + token + ":port:*", "node:" + token + ":server:*"} {
		iter := rdb.Scan(ctx, 0, pattern, 500).Iterator()
		var keys []string
		for iter.Next(ctx) {
			keys = append(keys, iter.Val())
		}
		if err := iter.Err(); err != nil {
			log.Printf("node teardown: per-server keys for node token %.8s... could not be listed: %v", token, err)
		}
		if len(keys) == 0 {
			continue
		}
		if err := rdb.Del(ctx, keys...).Err(); err != nil {
			log.Printf("node teardown: per-server keys for node token %.8s... could not be removed: %v", token, err)
		}
	}
}
