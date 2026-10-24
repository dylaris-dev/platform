package servers

import (
	"context"
	"testing"

	"dylaris-core/handlers"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func subServerInventoryHandler(t *testing.T) (*ServerHandler, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return &ServerHandler{state: &handlers.AppState{Redis: rdb}}, mr
}

const invUUID = "11111111-1111-4111-8111-111111111111"
const invKey = "dylaris:server:" + invUUID + ":stats:disk"

// "Known" and "empty" are different answers and the callers act on them in
// opposite directions, so every state the node's report can be in is pinned
// here - including the one that caused the bug, where the map is present in the
// JSON but null because the node's non-quota path never filled it.
func TestKnownSubServers(t *testing.T) {
	tests := []struct {
		name    string
		seed    string // "" = do not write the key at all
		wantOK  bool
		wantSet []string
		why     string
	}{
		{
			name:   "no report at all",
			seed:   "",
			wantOK: false,
			why:    "the key expires ten minutes after the last report; a cold cache is not an empty server",
		},
		{
			name:   "a report whose map is null",
			seed:   `{"total":123,"limit":456,"subServers":null,"enforceable":false}`,
			wantOK: false,
			why:    "this is what the node's du path used to send; treating it as an empty inventory is what made the limit unenforceable and would now refuse every switch",
		},
		{
			name:   "unparseable report",
			seed:   `not json`,
			wantOK: false,
			why:    "a garbled report is no answer, not an empty one",
		},
		{
			name:    "a populated report",
			seed:    `{"total":1,"limit":2,"subServers":{"survival":100,"creative":200}}`,
			wantOK:  true,
			wantSet: []string{"survival", "creative"},
			why:     "this is the only state in which a caller may refuse anything",
		},
		{
			name:    "a report with an empty map",
			seed:    `{"total":1,"limit":2,"subServers":{}}`,
			wantOK:  true,
			wantSet: nil,
			why:     "the node looked and found nothing, which is a real answer - unlike a missing map",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, mr := subServerInventoryHandler(t)
			if tt.seed != "" {
				mr.Set(invKey, tt.seed)
			}
			known, ok := h.knownSubServers(context.Background(), invUUID)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v: %s", ok, tt.wantOK, tt.why)
			}
			if !ok {
				return
			}
			if len(known) != len(tt.wantSet) {
				t.Fatalf("known = %v, want %v", known, tt.wantSet)
			}
			for _, name := range tt.wantSet {
				if !known[name] {
					t.Errorf("known is missing %q: %v", name, known)
				}
			}
		})
	}
}

// A nil Redis is the self-host / degraded case. It must read as "no answer" and
// not as "this server has no sub-servers", or a Redis outage would start
// refusing switches on every server at once.
func TestKnownSubServersWithoutRedis(t *testing.T) {
	h := &ServerHandler{state: &handlers.AppState{}}
	if _, ok := h.knownSubServers(context.Background(), invUUID); ok {
		t.Error("a missing Redis reported a known inventory")
	}
}
