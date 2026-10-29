package services

import (
	"context"
	"testing"

	"dylaris-core/store"
)

func TestEditionValue(t *testing.T) {
	for _, c := range []struct {
		java, bedrock, active bool
		want                  string
	}{
		{true, false, true, ""},
		{true, true, true, "both"},
		{false, true, true, "bedrock"},
		{true, true, false, ""},      // paused: Java only
		{false, true, false, "none"}, // paused Bedrock-only: nobody, by the owner's choice
		{false, false, true, "none"},
	} {
		if got := EditionValue(c.java, c.bedrock, c.active); got != c.want {
			t.Errorf("EditionValue(%v, %v, %v) = %q, want %q", c.java, c.bedrock, c.active, got, c.want)
		}
	}
}

func TestPublishEditionsWritesTheHashAndKeysOnce(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newBedrockFake()
	fs.eds = []store.RouteEdition{
		{Domain: "both.example.com", ServerID: 7, ServerUUID: "uuid-7", Java: true, Bedrock: true},
		{Domain: "be.example.com", ServerID: 7, ServerUUID: "uuid-7", Java: false, Bedrock: true},
	}
	mr.HSet(RouteEditionsKey, "stale.example.com", "both")
	svc := NewBedrockService(fs, rdb)
	if err := svc.PublishEditions(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet(RouteEditionsKey, "both.example.com"); got != "both" {
		t.Errorf("both.example.com = %q", got)
	}
	if got := mr.HGet(RouteEditionsKey, "be.example.com"); got != "bedrock" {
		t.Errorf("be.example.com = %q", got)
	}
	if mr.HGet(RouteEditionsKey, "stale.example.com") != "" {
		t.Error("a field without a row survived")
	}
	key := fs.keys[7]
	if len(key) != 16 || !mr.Exists(FloodgateKeyRedisKey("uuid-7")) {
		t.Fatal("a server with a Bedrock route got no key")
	}

	// Off: Bedrock pauses, the stored key stays (no rotation, no restart).
	if err := svc.PublishEditions(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet(RouteEditionsKey, "be.example.com"); got != "none" {
		t.Errorf("paused Bedrock-only = %q, want none", got)
	}
	if mr.HGet(RouteEditionsKey, "both.example.com") != "" {
		t.Error("a paused Java+Bedrock route must read as Java only")
	}
	if string(fs.keys[7]) != string(key) {
		t.Error("the key changed")
	}

	fs.eds = nil
	if err := svc.PublishEditions(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if mr.Exists(RouteEditionsKey) {
		t.Error("no rows must leave no hash")
	}
}
