package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// N18: a running container keeps the REDIS_ADDR it was created with until the
// node restarts, so a runtime promotion must not make the egress chain drop it.
// The state remembers every address it has handed out, oldest first.
func TestRedisAddrStateRemembersEveryAddressItUsed(t *testing.T) {
	s := &redisAddrState{
		probe: func(context.Context, string, []byte) error { return nil },
		save:  func(string) error { return nil },
	}
	s.set("redis:6379", true)
	s.persistIfNew()
	s.offer(context.Background(), "10.0.0.10:6379", nil)
	s.offer(context.Background(), "redis:6379", nil)
	want := []string{"redis:6379", "10.0.0.10:6379"}
	if got := s.used(); !reflect.DeepEqual(got, want) {
		t.Errorf("used() = %v, want %v", got, want)
	}
}

// Both the address older containers carry and the current one are accepted,
// and both before the private-range drop; nothing else private is opened.
func TestTheEgressChainAcceptsEveryRedisContainersCarry(t *testing.T) {
	rs := renderNetPolicy(true, nil, nil, &netEgress{
		overlay: "10.20.0.0/16", redisIP: "10.0.0.20", redisPort: 6379, gamePort: 25565, gateway: "10.20.0.1",
		priorRedis: []redisDest{{ip: "10.0.0.10", port: 6379}, {ip: "x; flush ruleset", port: 6379}, {ip: "10.0.0.11", port: 0}},
	})
	drop := strings.Index(rs, "ip daddr { 10.0.0.0/8")
	for _, line := range []string{"ip daddr 10.0.0.20 tcp dport 6379 accept", "ip daddr 10.0.0.10 tcp dport 6379 accept"} {
		i := strings.Index(rs, line)
		if i < 0 || drop < 0 || i > drop {
			t.Errorf("%q missing or after the private drop:\n%s", line, rs)
		}
	}
	if strings.Contains(rs, "flush") || strings.Contains(rs, "10.0.0.11") {
		t.Errorf("an invalid prior address was rendered:\n%s", rs)
	}
}
