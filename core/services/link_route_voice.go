package services

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"

	"dylaris-core/store"
)

// Voice for a route-only address. The relay needs no change: it hashes
// server_uuid into the wire's server id and meters under
// dylaris:traffic:edge:<server_uuid>, both treating the string as opaque. So a
// route gets a subject of its own in place of a server uuid, unique per domain
// (two voice routes of one owner must not share a wire id) and under the
// owner: prefix, which is what makes its bytes route-only traffic of that
// owner (trafficProductOf, subjectOwners). The customer's link reads its
// config from Core (LinkVoice), so there is no dylaris:server:<uuid>:voice key.

// LinkRouteVoiceSubject is the relay's server_uuid for a route-only domain.
func LinkRouteVoiceSubject(ownerID, domain string) string {
	return trafficOwnerSubjectPrefix + ownerID + ":voice:" + domain
}

// PublishLinkRouteVoice writes the relay's two keys for one route's port.
func PublishLinkRouteVoice(ctx context.Context, rdb *redis.Client, v store.LinkRouteVoice) error {
	b, err := json.Marshal(voicePortEntry{V: 1, ServerUUID: LinkRouteVoiceSubject(v.OwnerID, v.Domain), Key: hex.EncodeToString(v.UplinkKey)})
	if err != nil {
		return err
	}
	pipe := rdb.TxPipeline()
	pipe.Set(ctx, VoicePortKey(v.Region, v.Port), b, 0)
	pipe.SAdd(ctx, VoiceIndexKey(v.Region), strconv.Itoa(v.Port))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("publish route voice port %s:%d: %w", v.Region, v.Port, err)
	}
	return nil
}

// UnpublishLinkRouteVoice removes them.
func UnpublishLinkRouteVoice(ctx context.Context, rdb *redis.Client, v store.LinkRouteVoice) error {
	pipe := rdb.TxPipeline()
	pipe.Del(ctx, VoicePortKey(v.Region, v.Port))
	pipe.SRem(ctx, VoiceIndexKey(v.Region), strconv.Itoa(v.Port))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("unpublish route voice port %s:%d: %w", v.Region, v.Port, err)
	}
	return nil
}
