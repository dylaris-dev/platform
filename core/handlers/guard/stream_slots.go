package guard

import (
	"net/http"
	"sync"

	"dylaris-core/handlers/httpx"
	"dylaris-core/services/demo"
	"dylaris-core/store"
)

// Live streams one user may hold open per kind, on one Core replica. A panel
// tab holds one events stream plus at most two others, so these leave room for
// a dozen tabs and more. The bound is what matters: each console stream holds
// a Redis connection for its whole life, and with no cap one account with
// console.read could script enough of them to starve every console of every
// other user.
const (
	ConsoleStreamsPerUser = 8
	StatsStreamsPerUser   = 8
	EventStreamsPerUser   = 16
)

var liveStreams = streamSlots{held: map[string]int{}}

type streamSlots struct {
	mu   sync.Mutex
	held map[string]int
}

func (s *streamSlots) acquire(key string, max int) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[key] >= max {
		return nil, false
	}
	s.held[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.held[key]--; s.held[key] <= 0 {
				delete(s.held, key)
			}
		})
	}, true
}

// AcquireLiveStream takes one of the caller's slots for kind, or answers 429.
// Called before any stream header goes out. The browser closes an EventSource
// for good on a non-200, and the panel opens a fresh one on its own backoff.
//
// Every visitor of the public demo is the same account, so demo slots are
// counted per client address instead: per account, the sixteenth demo tab on a
// replica would have stopped every other demo visitor's live updates.
func AcquireLiveStream(w http.ResponseWriter, r *http.Request, st store.Store, kind string, max int) (release func(), ok bool) {
	userID, _ := r.Context().Value("userID").(string)
	holder := userID
	if st != nil && userID != "" {
		if demo, _ := st.GetSetting(demo.AccountUUIDSetting); demo == userID {
			holder = "demo@" + httpx.ClientIP(r)
		}
	}
	release, ok = liveStreams.acquire(kind+":"+holder, max)
	if !ok {
		httpx.SendJSONError(w, "Too many open live views - close some panel tabs", http.StatusTooManyRequests)
	}
	return release, ok
}
