package services

import (
	"context"
	"sync"

	"github.com/redis/go-redis/v9"
)

// hubSubscriberBuffer is how far one stream may fall behind before its
// messages are dropped. Matches go-redis's own per-PubSub channel size, which
// is what every stream had before the hub.
const hubSubscriberBuffer = 100

// PubSubHub multiplexes every SSE stream of one Core onto ONE Redis Pub/Sub
// connection. go-redis gives each PubSub its own connection, so a stream per
// subscription meant two Valkey connections per open panel tab (events plus
// stats), measured in the 2026-11-02 stress test. The hub subscribes a
// channel once, fans its messages out in process, and unsubscribes when the
// last stream on it leaves. Authorization stays with each stream: the hub
// only moves bytes, the handler still decides what it may forward.
type PubSubHub struct {
	rdb *redis.Client

	mu   sync.Mutex
	ps   *redis.PubSub // nil while nobody listens; the connection goes with it
	subs map[string]map[chan string]struct{}
}

func NewPubSubHub(rdb *redis.Client) *PubSubHub {
	return &PubSubHub{rdb: rdb, subs: map[string]map[chan string]struct{}{}}
}

// Subscribe returns the messages published on channel from now on, and the
// function that ends the subscription. A subscriber that falls more than
// hubSubscriberBuffer messages behind loses the overflow rather than stalling
// every other stream on the connection. The returned channel is never closed;
// callers stop on their own context.
func (h *PubSubHub) Subscribe(channel string) (<-chan string, func()) {
	ch := make(chan string, hubSubscriberBuffer)

	h.mu.Lock()
	if h.ps == nil {
		// Background context: a request ending mid-write must not break the
		// connection every other stream shares. A failed SUBSCRIBE is retried
		// by go-redis on reconnect, as it was for the per-stream PubSub.
		h.ps = h.rdb.Subscribe(context.Background(), channel)
		go h.run(h.ps)
	} else if h.subs[channel] == nil {
		_ = h.ps.Subscribe(context.Background(), channel)
	}
	if h.subs[channel] == nil {
		h.subs[channel] = map[chan string]struct{}{}
	}
	h.subs[channel][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() { once.Do(func() { h.unsubscribe(channel, ch) }) }
}

func (h *PubSubHub) unsubscribe(channel string, ch chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.subs[channel]
	delete(set, ch)
	if len(set) > 0 {
		return
	}
	delete(h.subs, channel)
	if len(h.subs) == 0 {
		// Closed rather than left idle: no stream is open, so the connection
		// has nothing to carry, and run() ends with it.
		_ = h.ps.Close()
		h.ps = nil
		return
	}
	_ = h.ps.Unsubscribe(context.Background(), channel)
}

func (h *PubSubHub) run(ps *redis.PubSub) {
	for msg := range ps.Channel(redis.WithChannelSize(1000)) {
		h.mu.Lock()
		for ch := range h.subs[msg.Channel] {
			select {
			case ch <- msg.Payload:
			default:
			}
		}
		h.mu.Unlock()
	}
}

// listening reports how many channels the hub holds open (tests).
func (h *PubSubHub) listening() (channels int, connected bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs), h.ps != nil
}
