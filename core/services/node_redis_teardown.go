package services

import (
	"context"
	"log"

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
		"dylaris:node:" + token + ":storage_placement",
		"dylaris:node:" + token + ":cpu",
		"dylaris:node:" + token + ":cpu:sig",
		"dylaris:node:" + token + ":cmds",
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
}
