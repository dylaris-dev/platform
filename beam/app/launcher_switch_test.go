package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func isolateSettings(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("AppData", tmp)
	t.Setenv("LocalAppData", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)
}

// The switch route needs no shell token, which is only sound because it can
// reach nothing but a panel the user already saved.
func TestSwitchRouteOnlyReachesSavedPanels(t *testing.T) {
	isolateSettings(t)
	if err := saveSettings(userSettings{Panels: []savedPanel{{Name: "Home", URL: "https://home.example"}}}); err != nil {
		t.Fatal(err)
	}
	h := newPanelMiddleware(&App{}, http.NotFoundHandler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, beamSwitchRoute+"?to=https://evil.example", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unsaved panel was accepted: %d", rec.Code)
	}
	if got := loadSettings().activePanel().URL; got == "https://evil.example" {
		t.Fatal("the refused switch still changed the active panel")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, beamSwitchRoute+"?to=https://home.example/", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("switch to a saved panel: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := loadSettings().activePanel().URL; got != "https://home.example" {
		t.Fatalf("active panel = %q after the switch", got)
	}
}

// What the launcher shows comes from the stamped state, so the state has to be
// in the tag - and encoded so no value can end the script element.
func TestLauncherStampsPanelsAndUpdate(t *testing.T) {
	got := launcherTag("", launcherState{
		Update:  "1.2.3",
		Current: "1.2.2",
		Panels:  []launcherPanel{{Name: "</script><b>", Host: "a", URL: "https://a"}, {Name: "B", Host: "b", URL: "https://b"}},
		Active:  "https://a",
	})
	for _, want := range []string{`"update":"1.2.3"`, `"current":"1.2.2"`, `"active":"https://a"`, `"switchRoute":"` + beamSwitchRoute + `"`} {
		if !strings.Contains(got, want) {
			t.Errorf("state is missing %s", want)
		}
	}
	if strings.Contains(got, "</script><b>") {
		t.Error("a panel name closed the script element")
	}
	if strings.Contains(got, "__STATE__") {
		t.Error("the state placeholder survived")
	}
}

func TestUpdateCheckRunsOnTheFullHour(t *testing.T) {
	at := time.Date(2026, 11, 3, 14, 7, 30, 0, time.UTC)
	if got := nextFullHour(at); !got.Equal(time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("next check at %v", got)
	}
	if got := updatePromptJS(`1.0"; alert(1); "`); strings.Contains(got, `"; alert(1); "`) {
		t.Fatalf("the version was not encoded: %s", got)
	}
}
