package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Noticing an update while the window is showing the panel.
//
// GetUpdateInfo was called once, from the settings screen's mount effect. That
// is the only screen the app shows before it hands the window to the panel, so
// an app left running for a week never looked again - and even when it did, the
// notice lived on a screen the user was no longer on.
//
// A ticker re-checks at startup and on every full hour and caches the answer.
// The launcher injected into every proxied page shows a dot while an update is
// waiting, and a prompt card: on page load unless the user said "Later" for that
// version in this session, and again after every hourly check that still finds
// one. A mandatory update keeps its own blocking screen.

// nextFullHour is when the next hourly check is due.
func nextFullHour(now time.Time) time.Time {
	return now.Truncate(time.Hour).Add(time.Hour)
}

// updateWatcher caches the waiting version for readers that must not block.
//
// The launcher tag is built while a proxied response is being rewritten, which
// is on the request path: asking the network there would stall every page load
// behind a release-host round trip, and fail the page when the host is down.
type updateWatcher struct {
	mu     sync.RWMutex
	latest string
}

// get reports the newer version waiting, or "" when none is known. Empty until
// the first check completes: a prompt shown before anything is known would be a
// lie.
func (w *updateWatcher) get() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.latest
}

func (w *updateWatcher) set(v string) {
	w.mu.Lock()
	w.latest = v
	w.mu.Unlock()
}

// updatePromptJS asks the launcher on the current page to show its prompt.
// Guarded, because the window may be on the settings page or mid-navigation,
// where no launcher exists.
func updatePromptJS(version string) string {
	v, _ := json.Marshal(version)
	return "window.__beamUpdatePrompt && window.__beamUpdatePrompt(" + string(v) + ")"
}

// startUpdateWatch runs the check now and then on every full hour until ctx ends.
//
// Errors are swallowed on purpose: the app keeps working and the next hour tries
// again. A failed fetch leaves the cached answer alone rather than clearing it,
// so a network blip does not make a waiting update vanish.
func (a *App) startUpdateWatch(ctx context.Context) {
	check := func() {
		info := a.GetUpdateInfo()
		if info == nil || info.Latest == "" {
			return
		}
		if !info.UpdateAvailable {
			a.updates.set("")
			return
		}
		a.updates.set(info.Latest)
		wailsruntime.WindowExecJS(ctx, updatePromptJS(info.Latest))
	}
	go func() {
		check()
		for {
			t := time.NewTimer(time.Until(nextFullHour(time.Now())))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				check()
			}
		}
	}()
}
