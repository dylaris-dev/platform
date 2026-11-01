package handlers

import (
	"bytes"
	"log"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestNewSetupCodeFormat(t *testing.T) {
	g := `[` + setupCodeAlphabet + `]{4}`
	shape := regexp.MustCompile(`^` + g + `-` + g + `-` + g + `$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := newSetupCode()
		if err != nil {
			t.Fatal(err)
		}
		if !shape.MatchString(c) {
			t.Fatalf("code %q is not XXXX-XXXX-XXXX from the alphabet", c)
		}
		if strings.ContainsAny(c, "01ILO") {
			t.Fatalf("code %q holds an ambiguous character", c)
		}
		seen[c] = true
	}
	if len(seen) < 200 {
		t.Fatalf("only %d distinct codes in 200", len(seen))
	}
}

func TestSetupCodeMatches(t *testing.T) {
	stored := hashSetupCode("ABCD-EFGH-JKMN")
	for _, tc := range []struct {
		stored, provided string
		want             bool
	}{
		{stored, "ABCD-EFGH-JKMN", true},
		{stored, "abcd-efgh-jkmn", true},
		{stored, " ABCDEFGHJKMN ", true},
		{stored, "ABCD-EFGH-JKMP", false},
		{stored, "", false},
		{stored, "---", false},
		{"", "", false},
		{"", "ABCD-EFGH-JKMN", false},
	} {
		if got := setupCodeMatches(tc.stored, tc.provided); got != tc.want {
			t.Errorf("setupCodeMatches(%q, %q) = %v, want %v", tc.stored, tc.provided, got, tc.want)
		}
	}
}

// ensureFakeStore has the insert-if-absent semantics of the real ON CONFLICT
// DO NOTHING, under a lock so concurrent boots race like replicas do.
type ensureFakeStore struct {
	mu       sync.Mutex
	users    int
	settings map[string]string
}

func (f *ensureFakeStore) CountUsers() (int, error) { return f.users, nil }

func (f *ensureFakeStore) InsertSettingIfAbsent(key, value string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.settings[key]; ok {
		return false, nil
	}
	f.settings[key] = value
	return true, nil
}

func (f *ensureFakeStore) DeleteSetting(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.settings, key)
	return nil
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// Several replicas booting a fresh install at once: exactly one code is stored,
// exactly one is logged, and the logged one is the stored one.
func TestEnsureSetupCodeSingleWinner(t *testing.T) {
	buf := captureLog(t)
	st := &ensureFakeStore{settings: map[string]string{}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := EnsureSetupCode(st); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// A later restart keeps the stored code and logs no new one.
	if err := EnsureSetupCode(st); err != nil {
		t.Fatal(err)
	}

	codes := regexp.MustCompile(`SETUP CODE: (\S+)`).FindAllStringSubmatch(buf.String(), -1)
	if len(codes) != 1 {
		t.Fatalf("%d codes logged, want 1:\n%s", len(codes), buf.String())
	}
	if !setupCodeMatches(st.settings[SetupCodeSettingKey], codes[0][1]) {
		t.Fatal("the logged code is not the stored one")
	}
	if n := strings.Count(buf.String(), "still unused"); n != 8 {
		t.Fatalf("%d replicas said a code exists, want 8 (7 losers and the restart):\n%s", n, buf.String())
	}
}

func TestEnsureSetupCodeRemovedOnceUsersExist(t *testing.T) {
	buf := captureLog(t)
	st := &ensureFakeStore{users: 1, settings: map[string]string{SetupCodeSettingKey: "leftover"}}
	if err := EnsureSetupCode(st); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.settings[SetupCodeSettingKey]; ok {
		t.Fatal("a leftover code survived on an installed platform")
	}
	if strings.Contains(buf.String(), "SETUP CODE") {
		t.Fatalf("logged a code on an installed platform: %s", buf.String())
	}
}
