package guard

import "testing"

func TestStreamSlots(t *testing.T) {
	s := streamSlots{held: map[string]int{}}
	var releases []func()
	for range 3 {
		rel, ok := s.acquire("console:u1", 3)
		if !ok {
			t.Fatal("refused under the cap")
		}
		releases = append(releases, rel)
	}
	if _, ok := s.acquire("console:u1", 3); ok {
		t.Fatal("a fourth stream got past a cap of three")
	}
	if _, ok := s.acquire("console:u2", 3); !ok {
		t.Fatal("one user's streams counted against another")
	}
	releases[0]()
	releases[0]() // twice is once
	if _, ok := s.acquire("console:u1", 3); !ok {
		t.Fatal("a released slot was not given back")
	}
	if _, ok := s.acquire("console:u1", 3); ok {
		t.Fatal("a double release handed out an extra slot")
	}
}
