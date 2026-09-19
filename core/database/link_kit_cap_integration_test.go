package database

import (
	"sync"
	"testing"

	"dylaris-core/store"
)

// Two mints racing for the last slot. The count and the insert used to be two
// statements, so both saw room and both succeeded - and a tenant over their cap
// is one the over-limit sweep stops entirely three days later.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationLinkKitCapHoldsUnderARace(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	const racers = 8
	var wg sync.WaitGroup
	results := make(chan bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := st.CreateLinkKitUnderCap(store.WarpAPIKey{
				Name: "kit", KeyHash: uniqueName("h_"), Policy: "general", MaxConns: 1,
				OnNewConn: "kill_old", NodeID: uniqueName("link-"), OwnerID: f.user.ID,
			}, 1)
			if err != nil {
				t.Errorf("CreateLinkKitUnderCap: %v", err)
			}
			results <- ok
		}()
	}
	wg.Wait()
	close(results)
	created := 0
	for ok := range results {
		if ok {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d kits created under a cap of 1", created)
	}
	if n, err := st.CountLinkKitsByOwner(f.user.ID); err != nil || n != 1 {
		t.Errorf("live kits = (%d, %v), want 1", n, err)
	}
}
