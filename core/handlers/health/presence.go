package health

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// How many people were using the platform at the same time is one of the
// numbers a record like this exists to answer, and Core had no way to say it:
// sessions are stateless JWTs, so there is no session table to count and
// last_login_at cannot distinguish "logged in this morning" from "here now".
//
// The panel holds one SSE stream open for as long as it is open, which makes
// that stream the honest definition of "here now".
//
// Counted through REDIS rather than in memory, and that is the part that
// matters: Core runs several replicas, each terminating its own streams, while
// the metrics collector is leader-gated. An in-process counter would report
// only the leader's share - and a per-replica count of distinct users cannot be
// summed either, because one person with two tabs on two replicas would count
// twice. A per-user key with a TTL is exact without any coordination: both
// replicas write the same key, the count is the number of keys, and a user who
// closes the tab disappears when it expires.
const (
	presenceUserPrefix = "dylaris:presence:user:"
	presenceStreamsKey = "dylaris:presence:streams:"

	// Longer than the refresh interval so a slow tick does not blink someone
	// out of existence, short enough that a closed tab stops counting quickly.
	presenceTTL = 90 * time.Second

	presenceRefresh = 15 * time.Second
)

// openStreams is this process's live SSE stream count. Per-replica and
// published under its own key, because streams and PEOPLE are different
// numbers: one person with three tabs is three streams.
var openStreams atomic.Int64

// presenceTrack marks a user present for as long as the caller holds an SSE
// stream, and keeps this replica's stream count published.
//
// Best-effort throughout. Presence is a statistic; a Redis error must never
// affect whether the stream itself works.
func presenceTrack(ctx context.Context, rdb *redis.Client, coreID, userID string) func() {
	openStreams.Add(1)
	stop := make(chan struct{})

	refresh := func() {
		if rdb == nil {
			return
		}
		if userID != "" {
			rdb.Set(ctx, presenceUserPrefix+userID, "1", presenceTTL)
		}
		rdb.Set(ctx, presenceStreamsKey+coreID, openStreams.Load(), presenceTTL)
	}
	refresh()

	go func() {
		t := time.NewTicker(presenceRefresh)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()

	return func() {
		close(stop)
		n := openStreams.Add(-1)
		// The stream count is corrected immediately on disconnect; the per-user
		// key is deliberately left to EXPIRE rather than deleted, because this
		// user may still hold another stream - here or on another replica - and
		// deleting it would report them as gone while they are still watching.
		if rdb != nil {
			rdb.Set(context.WithoutCancel(ctx), presenceStreamsKey+coreID, n, presenceTTL)
		}
	}
}
