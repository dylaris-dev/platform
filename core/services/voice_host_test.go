package services

import (
	"context"
	"testing"
)

func TestVoiceHostFor(t *testing.T) {
	eu := GatewayEdgeInfo{EdgeID: "e1", Status: "online", Region: "eu", Wildcard: "*.eu.dylaris.com"}
	eu2 := GatewayEdgeInfo{EdgeID: "e2", Status: "online", Region: "eu", Wildcard: "*.EU.dylaris.com"}
	euOther := GatewayEdgeInfo{EdgeID: "e3", Status: "online", Region: "eu", Wildcard: "*.a-eu.dylaris.net"}
	offline := GatewayEdgeInfo{EdgeID: "e4", Status: "offline", Region: "us", Wildcard: "*.us.dylaris.com"}
	bare := GatewayEdgeInfo{EdgeID: "e5", Status: "online", Region: "ap", Wildcard: "ap.dylaris.com"}
	cases := []struct {
		name   string
		region string
		edges  []GatewayEdgeInfo
		want   string
		ok     bool
	}{
		{"one wildcard", "eu", []GatewayEdgeInfo{eu}, "voice.eu.dylaris.com", true},
		{"two edges of one region, same wildcard", "eu", []GatewayEdgeInfo{eu2, eu}, "voice.eu.dylaris.com", true},
		{"two wildcards, the lexically first", "eu", []GatewayEdgeInfo{eu, euOther}, "voice.a-eu.dylaris.net", true},
		{"offline edge only", "us", []GatewayEdgeInfo{offline}, "", false},
		{"unknown region", "sa", []GatewayEdgeInfo{eu}, "", false},
		{"no *. prefix", "ap", []GatewayEdgeInfo{bare}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := VoiceHostFor(tc.region, tc.edges)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got %q %v, want %q %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// GatewayRouteCovers is the pointing check's admission in production: only a
// name that is, or lies under, a live route is ever resolved.
func TestGatewayRouteCovers(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	mr.Set("route:play.example.com", `{}`)
	cases := []struct {
		name string
		want bool
	}{
		{"play.example.com", true},
		{"Play.Example.com.", true},
		{"voice.play.example.com", true},
		{"example.com", false},
		{"other.example.com", false},
		{"evilplay.example.com", false},
		{"xplay.example.com", false},
		{"sub.play.example.com", true},
		{"metadata.google.internal", false},
		{"localhost", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GatewayRouteCovers(context.Background(), rdb, tc.name)
			if err != nil || got != tc.want {
				t.Fatalf("got %v %v, want %v", got, err, tc.want)
			}
		})
	}
	t.Run("Redis down is an error, not a no", func(t *testing.T) {
		mr.Close()
		if _, err := GatewayRouteCovers(context.Background(), rdb, "play.example.com"); err == nil {
			t.Fatal("no error with Redis down")
		}
	})
}
