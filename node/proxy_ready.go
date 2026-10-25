package main

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Whether a server expects a PROXY protocol header, as its own status ping
// answered. The node pings with the header when Core's proxy_protocol key is
// set and without it otherwise; when that ping fails it tries the other way
// once, and whichever answered is what the server expects. That works for any
// software or plugin that reads PROXY (Paper's setting, a Forge mod, one
// nobody listed), which is the point: the owner installs whatever they like
// and Core forwards the player's address once the server shows it can take it.
//
// dylaris:server:<uuid>:proxy_ready = "1" (expects the header) or "0" (does
// not). Read by core/services/proxy_protocol_auto.go. No key means "not
// known", and Core then changes nothing.

const (
	proxyReadyTTL     = 10 * time.Minute
	proxyReadyRefresh = 2 * time.Minute
)

func proxyReadyKey(uuid string) string { return "dylaris:server:" + uuid + ":proxy_ready" }

type proxyReadyReporter struct {
	mu      sync.Mutex
	known   bool
	expects bool
	written time.Time
}

// report records what answered and writes the key when the answer changed or
// the last write is getting old.
func (p *proxyReadyReporter) report(ctx context.Context, rdb *redis.Client, uuid string, expects bool, now time.Time) {
	p.mu.Lock()
	due := !p.known || p.expects != expects || now.Sub(p.written) >= proxyReadyRefresh
	if due {
		p.known, p.expects, p.written = true, expects, now
	}
	p.mu.Unlock()
	if !due || rdb == nil {
		return
	}
	v := "0"
	if expects {
		v = "1"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rdb.Set(ctx, proxyReadyKey(uuid), v, proxyReadyTTL)
}

// pingEitherWay pings with the header if preferProxy, else without; on a
// failure it tries the other way once. It returns the response and whether
// the answering ping carried the header.
func pingEitherWay(ping func(proxy bool) (*SLPResponse, error), preferProxy bool) (*SLPResponse, bool, error) {
	resp, err := ping(preferProxy)
	if err == nil {
		return resp, preferProxy, nil
	}
	if resp2, err2 := ping(!preferProxy); err2 == nil {
		return resp2, !preferProxy, nil
	}
	return nil, preferProxy, err
}
