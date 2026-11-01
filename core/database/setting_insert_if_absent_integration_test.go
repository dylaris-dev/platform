package database

import (
	"fmt"
	"sync"
	"testing"
)

// The setup code relies on this: replicas booting together each try to store
// their own code, and exactly one insert may land. Skipped without
// DYLARIS_TEST_DB_HOST.
func TestIntegrationInsertSettingIfAbsentHasOneWinner(t *testing.T) {
	_, st := integrationDB(t)
	key := uniqueName("it_insert_if_absent_")
	t.Cleanup(func() { _ = st.DeleteSetting(key) })

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(v string) {
			defer wg.Done()
			won, err := st.InsertSettingIfAbsent(key, v)
			if err != nil {
				t.Error(err)
				return
			}
			if won {
				mu.Lock()
				winners = append(winners, v)
				mu.Unlock()
			}
		}(fmt.Sprintf("value-%d", i))
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("%d inserts landed, want 1: %v", len(winners), winners)
	}
	if got, err := st.GetSetting(key); err != nil || got != winners[0] {
		t.Fatalf("stored %q, %v; want the winner's %q", got, err, winners[0])
	}

	if err := st.DeleteSetting(key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSetting(key); err == nil {
		t.Fatal("the row survived DeleteSetting")
	}
	if err := st.DeleteSetting(key); err != nil {
		t.Fatalf("deleting a missing row: %v", err)
	}
}
