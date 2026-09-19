package handlers

import (
	"net/http/httptest"
	"testing"
	"time"
)

// A maintenance window must not take the player path down. The customer's warp
// and link call these with a kit key, not a login, so the admin pass never
// applied to them: route-only links went offline and restarting ones
// crash-looped for the whole window.
func TestMaintenanceNeverBlocksTheKitEndpoints(t *testing.T) {
	maintCacheMu.Lock()
	maintCache = MaintenanceState{Active: true, BlockLevel: "block_all"}
	maintCacheSet = true
	maintCacheExp = time.Now().Add(time.Hour)
	maintCacheMu.Unlock()
	t.Cleanup(func() {
		maintCacheMu.Lock()
		maintCacheSet = false
		maintCacheMu.Unlock()
	})

	for _, c := range []struct{ method, path string }{
		{"POST", "/api/warp/enroll"},
		{"GET", "/api/warp/assignment"},
		{"POST", "/api/warp/link-boot"},
		{"GET", "/api/warp/link/edges"},
		{"POST", "/api/warp/link/heartbeat"},
		{"POST", "/api/warp/link/stats"},
	} {
		if shouldBlockForMaintenance(nil, httptest.NewRequest(c.method, c.path, nil), false) {
			t.Errorf("%s %s is blocked by maintenance", c.method, c.path)
		}
	}
	// The exemption is for the machines, not for the people minting kits.
	if !shouldBlockForMaintenance(nil, httptest.NewRequest("POST", "/api/warp/link-kits", nil), false) {
		t.Error("a user endpoint under /api/warp slipped through maintenance")
	}
}
