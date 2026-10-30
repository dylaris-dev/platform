package services

import (
	"context"
	"fmt"
)

// RouteEditionsKey is the per-route edition hash the edge and the
// bedrock-bridge read (gateway repo, edge/cmd/route_editions.go, compiles its
// own reader): domain -> "both" | "bedrock" | "none". A domain without a
// field is Java only. Values are EFFECTIVE: the platform's Bedrock switch is
// already folded in, so the gateway needs no setting of its own.
const RouteEditionsKey = "sys:route:editions"

// EditionValue folds one route's stored editions with whether Bedrock is on
// for the platform. "" is Java only and is not written.
func EditionValue(java, bedrock, bedrockActive bool) string {
	b := bedrock && bedrockActive
	switch {
	case java && b:
		return "both"
	case b:
		return "bedrock"
	case java:
		return ""
	}
	return "none"
}

// BedrockActive is whether routes with Bedrock take Bedrock players now:
// the switch is anything but off. A nil flags reads as off.
func BedrockActive(ctx context.Context, f *FeatureFlags) bool {
	return f != nil && f.BedrockAudience(ctx) != "off"
}

// LinkRouteBedrockSubject is the name a route-only address's Floodgate key is
// published under, in place of a server uuid: the bridge reads
// dylaris:server:<subject>:floodgate (gateway repo, floodgateSubject, compiles
// its own copy). Under the owner: prefix like LinkRouteVoiceSubject, and per
// domain because every address has its own key.
func LinkRouteBedrockSubject(ownerID, domain string) string {
	return trafficOwnerSubjectPrefix + ownerID + ":bedrock:" + domain
}

// PublishEditions rewrites RouteEditionsKey from both tables in one MULTI, and
// gives every server with a Bedrock route, and every route-only address with
// a row, its Floodgate key. Keys are never
// taken away here: Floodgate reads the key at startup, so a key that changed
// on every off-and-on would need a restart each time, and access is decided
// by this hash, not by the key.
func (b *BedrockService) PublishEditions(ctx context.Context, active bool) error {
	rows, err := b.store.ListRouteEditions(0)
	if err != nil {
		return fmt.Errorf("list route editions: %w", err)
	}
	refs, err := b.store.ListBedrockServerRefs()
	if err != nil {
		return fmt.Errorf("list bedrock servers: %w", err)
	}
	hasKey := map[int]bool{}
	for _, r := range refs {
		hasKey[r.ServerID] = true
	}
	var fields []interface{}
	for _, r := range rows {
		if r.Bedrock && !hasKey[r.ServerID] {
			if err := b.Enable(ctx, r.ServerID, r.ServerUUID); err != nil {
				return err
			}
			hasKey[r.ServerID] = true
		}
		if v := EditionValue(r.Java, r.Bedrock, active); v != "" {
			fields = append(fields, r.Domain, v)
		}
	}
	links, err := b.store.ListLinkRouteEditions()
	if err != nil {
		return fmt.Errorf("list link route editions: %w", err)
	}
	for _, r := range links {
		// No key: unreadable at rest (logged by the store). The one already
		// in Redis stays; Reconcile keeps its name.
		if r.Key != nil {
			if err := b.publish(ctx, LinkRouteBedrockSubject(r.OwnerID, r.Domain), r.Key); err != nil {
				return err
			}
		}
		if v := EditionValue(r.Java, r.Bedrock, active); v != "" {
			fields = append(fields, r.Domain, v)
		}
	}
	pipe := b.redis.TxPipeline()
	pipe.Del(ctx, RouteEditionsKey)
	if len(fields) > 0 {
		pipe.HSet(ctx, RouteEditionsKey, fields...)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("publish route editions: %w", err)
	}
	return nil
}
