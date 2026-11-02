package corestorage

import (
	"sync"
	"time"
)

// Whether modpack storage resolves to a real provider, cached.
//
// The predicate itself has to stay the same call every write path makes -
// BuildModpackProvider - because a cheaper second opinion is exactly how
// a panel ends up disagreeing with the endpoint that answers 424. But that call
// is not free, and it ended up on GET /api/system/features, which every
// authenticated user hits at panel boot and again on every features.changed
// fan-out:
//
//   - a selected storage connection means a database round trip plus an AES-GCM
//     decrypt of the stored secret, per request, per user;
//   - the "core-storage" backend goes through BuildProvider, which
//     takes one of the shared storage-gate slots that file uploads and downloads
//     compete for, waits up to storageSlotWaitDeadline for one, and then calls
//     os.MkdirAll. A plain authenticated GET that can block for fifteen seconds
//     and creates a directory on a network mount is not a feature-flag read.
//
// So it is cached, next to the sixty-second cache every neighbouring flag in
// that payload already uses, and invalidated explicitly by the two writes that
// can change the answer. The panel refreshes its flags on modpack_settings.changed,
// so an operator who configures storage sees the modpack page unblock at once
// rather than at the end of a TTL.
const modpackStorageTTL = 60 * time.Second

type modpackStorageState struct {
	mu         sync.Mutex
	configured bool
	at         time.Time
	valid      bool
}

// ModpackConfigured reports whether modpack archives have somewhere to
// go. Same call every write path makes; the result is reused for a minute.
func (s *Manager) ModpackConfigured() bool {
	s.modpackStorage.mu.Lock()
	defer s.modpackStorage.mu.Unlock()

	if s.modpackStorage.valid && time.Since(s.modpackStorage.at) < modpackStorageTTL {
		return s.modpackStorage.configured
	}

	prov, err := s.BuildModpackProvider()
	s.modpackStorage.configured = err == nil && prov != nil
	s.modpackStorage.at = time.Now()
	s.modpackStorage.valid = true
	return s.modpackStorage.configured
}

// InvalidateModpack forgets the cached answer. Called by the writes that
// can change it: the modpack settings save, and any change to the storage
// connection a modpack setting may point at.
func (s *Manager) InvalidateModpack() {
	s.modpackStorage.mu.Lock()
	s.modpackStorage.valid = false
	s.modpackStorage.mu.Unlock()
}
