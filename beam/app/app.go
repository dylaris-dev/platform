package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the main Wails application struct.
// All methods on this struct are exposed to the frontend via bindings.
type App struct {
	ctx context.Context

	// connMu guards the session/connection pointers below. Wails dispatches
	// each frontend binding call on its own goroutine, and the health-check
	// goroutine touches them too, so client and relayClient are read and
	// written concurrently. The accessors keep critical sections to a single
	// field read/write and never hold the lock across network I/O. panelURL is
	// set once at startup and never mutated here, so it stays unguarded.
	connMu      sync.Mutex
	client      *CoreClient     // REST API client (login, config, tickets)
	relayClient *BeamNodeClient // gRPC client via BeamRelay (file ops)
	connMode    string          // active transport for the live tunnel: "lan-fastpath" | "relay" | "direct"; "" when not connected
	panelURL    string          // URL the webview navigates to on startup (Panel)
	apiURL      string          // Core API origin for the proxied Panel's CSP connect-src ("" = same-origin /api)

	// Active chunked uploads, keyed by an opaque JS-supplied uploadID.
	// Each value is the open gRPC stream session — the FileBrowser
	// uploads one file at a time so the map normally has 0 or 1 entries,
	// but we don't enforce that (canceling a stuck upload is racier
	// without a unique ID per attempt).
	uploadsMu sync.Mutex
	uploads   map[string]*UploadSession

	// cookies is the shell's cookie jar for the proxied Panel. The session is
	// an HttpOnly cookie and Beam keeps it here rather than in the webview;
	// see applyShellCookies in proxy.go.
	cookieMu sync.Mutex
	cookies  http.CookieJar

	// sessionFingerprint is the last cookie set written to disk. Guards the
	// write in persistPanelSession, which runs on every proxied response; see
	// session_store.go.
	sessionMu          sync.Mutex
	sessionFingerprint string

	// readable holds the Set-Cookie lines the webview IS allowed to have, keyed
	// by PANEL and then by cookie name, so they can be replayed into the page;
	// see readableCookieScript in proxy.go. Per panel because the app can hold
	// several: one flat map would write panel A's sign-in hint into panel B's
	// document, telling B's login screen a session exists when none does.
	readableMu sync.Mutex
	readable   map[string]map[string]string

	// updates caches whether a newer build is waiting, so the launcher injected
	// into every proxied page can render its dot without a network call on the
	// request path. See update_watch.go.
	updates updateWatcher

	// healthCheckStop is closed when the current health-check goroutine
	// should exit (Logout, SetSession on a new account). nil while no
	// check is running. The goroutine is owned exclusively by App and
	// kept lightweight — one Ping every 30s, skipped while uploads are
	// in flight so we never race the resume-retry loop on the JS side.
	healthCheckMu   sync.Mutex
	healthCheckStop chan struct{}

	// downloadMu guards lastDownloadPath, the destination the user picked
	// via the native OS save dialog in the most recent successful
	// DownloadFile/SelectiveDownload call. It is the only path
	// RevealInExplorer will ever open (see comment there).
	downloadMu       sync.Mutex
	lastDownloadPath string

	// gateMu guards the force-update gate. gateBlocked is set once the app knows
	// its build is below Core's advertised min_version (proactively after auth,
	// or reactively from a GetBeamTicket 426). While set, the app-shell shows the
	// mandatory-update screen and the proxy middleware pins the webview to it.
	gateMu      sync.Mutex
	gateBlocked bool
	gateMin     string

	// updateInFlight guards ApplyUpdate against concurrent/re-entrant runs: the
	// mandatory screen and the optional nag both call it, and a user can click
	// twice. CompareAndSwap admits exactly one flow at a time.
	updateInFlight atomic.Bool

	// updateChannel caches the EFFECTIVE update channel Core reported for the
	// logged-in user ("stable" | "dev"), set after GetBeamConfig. It selects which
	// signed release manifest the updater checks and drives the dev badge. Empty
	// until the first config fetch, which getUpdateChannel treats as "stable".
	updateChannel atomic.Value

	// shellToken is a per-run capability secret (32 random bytes, hex). It is
	// minted in NewApp, delivered ONLY to the first-party /__beam/ app-shell
	// page (spliced into its HTML by the proxy), and never sent to the proxied
	// Panel. The three side-effecting bound methods (SavePanelURL, ApplyUpdate,
	// OpenUpdateDownload) require it as their first argument. This RAISES THE BAR
	// against a blindly-injected Panel compromise; it is NOT a hard boundary,
	// because the proxied Panel shares this wails:// origin and a same-origin
	// fetch of /__beam/ could read the token unless the Sec-Fetch-Dest navigation
	// gate in serveBeamIndex blocks it (which relies on the webview sending Fetch
	// Metadata). A true boundary would require the native Wails dispatcher to check
	// the current top-level URL at call time (deferred).
	shellToken string
}

// userSettings is the schema of the JSON config file we persist between
// runs. Lives at $UserConfigDir/dylaris-beam/config.json — the user's
// Panel URL choice survives reinstalls of the same major version.
type userSettings struct {
	// PanelURL and APIURL are the pre-list schema, kept because installs in the
	// field have them on disk. Read only when Panels is empty; see panelList.
	PanelURL string `json:"panelUrl"`
	APIURL   string `json:"apiUrl"` // Core API origin ("" = same-origin /api)

	// Panels is every panel the app knows about, and Active is which of them the
	// window is showing (by URL, so reordering the list does not move it).
	Panels []savedPanel `json:"panels,omitempty"`
	Active string       `json:"activePanel,omitempty"`
}

// settingsPath returns the OS-appropriate config file location:
//
//	Windows: %AppData%\dylaris-beam\config.json
//	macOS:   ~/Library/Application Support/dylaris-beam/config.json
//	Linux:   ~/.config/dylaris-beam/config.json
//
// Errors fall back to a path next to the executable so the app stays
// functional even on locked-down systems.
func settingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		exe, _ := os.Executable()
		dir = filepath.Dir(exe)
	}
	return filepath.Join(dir, "dylaris-beam", "config.json")
}

func loadSettings() userSettings {
	var s userSettings
	data, err := os.ReadFile(settingsPath())
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s)
	s.PanelURL = strings.TrimSpace(s.PanelURL)
	s.APIURL = strings.TrimSpace(s.APIURL)
	return s
}

func saveSettings(s userSettings) error {
	path := settingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// GetPanelURL is exposed to the frontend stub so the embedded redirector
// knows where to point its window.location. Resolution priority:
//  1. Saved user setting (Settings dialog wrote it)
//  2. Launch/build-time default (DYLARIS_PANEL_URL env or ldflags, on the App struct)
//  3. The compiled-in defaultPanelURL (empty in the open-source build)
//
// May return "" when nothing is configured; the redirector then shows Settings.
func (a *App) GetPanelURL() string {
	// The ACTIVE entry of the list, which a pre-list config migrates into. Every
	// caller that used to read the single setting - the proxy target, the CSP
	// connect-src, the deploy kit's coreOrigin - follows a switch through here
	// without knowing the list exists.
	if active := strings.TrimSpace(loadSettings().activePanel().URL); active != "" {
		return active
	}
	if a.panelURL != "" {
		return a.panelURL
	}
	return defaultPanelURL
}

// GetDefaultPanelURL returns the URL the redirector would use absent a
// saved override. The Settings dialog uses it to pre-fill the input
// and to offer a "Reset" button. May be "" when no default is compiled in.
func (a *App) GetDefaultPanelURL() string {
	if a.panelURL != "" {
		return a.panelURL
	}
	return defaultPanelURL
}

// SavePanelURL persists a new Panel URL chosen via the Settings dialog.
// Pass an empty string to clear the override (falls back to the
// build-time default on the next launch). Requires the shell capability
// token (broker isolation): a compromised proxied Panel does not hold it,
// so it cannot repoint the app at an attacker origin. Load-modify-save so a
// co-existing APIURL override is preserved.
func (a *App) SavePanelURL(token, url string) error {
	if !a.checkShellToken(token) {
		return fmt.Errorf("unauthorized")
	}
	url = strings.TrimSpace(url)
	// Trim a trailing slash so the redirector's later concatenation
	// (e.g. "/login") never produces a double-slash.
	url = strings.TrimRight(url, "/")
	s := loadSettings()
	// Writes into the LIST, not beside it. Two places holding "which panel" is
	// two places that can disagree, and the one the proxy reads would win
	// silently.
	list := s.panelList()
	active := s.activePanel()
	replaced := false
	for i := range list {
		if list[i].URL == active.URL {
			list[i].URL = url
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, savedPanel{URL: url})
	}
	s.Panels, s.Active = list, url
	s.PanelURL, s.APIURL = "", ""
	// The shell holds the session for the panel it was pointed at. Repointing an
	// entry must not carry that credential to a different host: at best the new
	// one rejects it, at worst it is sent to someone who should never see it.
	a.forgetPanelSession()
	return saveSettings(s)
}

// ClearLocalData drops everything Beam keeps about the current panel on this
// machine: the session the shell holds and, with it, the signed-in state.
//
// It is the "clear cache" that actually means something here. Clearing site
// data inside the panel would reach nothing, because the credential was never
// in the webview - so this is the only way out of a half-broken session without
// reinstalling. Shell-token gated like the other side-effecting bindings.
func (a *App) ClearLocalData(token string) error {
	if !a.checkShellToken(token) {
		return fmt.Errorf("unauthorized")
	}
	a.forgetPanelSession()
	return nil
}

// GetAPIURL returns the Core API origin the proxied Panel is allowed to reach
// (CSP connect-src). Same priority as GetPanelURL: saved setting, then the
// launch/build-time default (DYLARIS_API_URL / ldflags), then defaultAPIURL.
// "" means the Panel talks to the API same-origin (/api) and needs no extra
// connect-src entry.
func (a *App) GetAPIURL() string {
	if saved := strings.TrimSpace(loadSettings().activePanel().APIURL); saved != "" {
		return saved
	}
	if a.apiURL != "" {
		return a.apiURL
	}
	return defaultAPIURL
}

// GetDefaultAPIURL returns the API origin the app would use absent a saved
// override. The Settings dialog uses it for the "Use default" button. May be
// "" when no default is compiled in (same-origin build).
func (a *App) GetDefaultAPIURL() string {
	if a.apiURL != "" {
		return a.apiURL
	}
	return defaultAPIURL
}

// SaveAPIURL persists the Core API origin chosen via the Settings dialog. Empty
// clears the override (same-origin /api). Shell-token gated like SavePanelURL,
// and load-modify-save so the PanelURL override is preserved.
func (a *App) SaveAPIURL(token, url string) error {
	if !a.checkShellToken(token) {
		return fmt.Errorf("unauthorized")
	}
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	s := loadSettings()
	// The override belongs to the ACTIVE panel; the entries are different
	// deployments, so a global one would follow the user to a panel it is wrong
	// for.
	list := s.panelList()
	active := s.activePanel()
	for i := range list {
		if list[i].URL == active.URL {
			list[i].APIURL = url
		}
	}
	s.Panels, s.Active = list, active.URL
	s.PanelURL, s.APIURL = "", ""
	if len(list) == 0 {
		s.APIURL = url
	}
	return saveSettings(s)
}

func NewApp() *App {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// The app cannot enforce broker isolation without a token; refuse to
		// start rather than silently run with an empty (guessable) one.
		panic("beam: failed to generate shell capability token: " + err.Error())
	}
	return &App{
		uploads:    make(map[string]*UploadSession),
		shellToken: hex.EncodeToString(buf),
	}
}

// checkShellToken reports whether tok matches this run's shell capability
// token, in constant time. ConstantTimeCompare returns 0 for any length
// mismatch, so an empty or truncated token is rejected.
func (a *App) checkShellToken(tok string) bool {
	return subtle.ConstantTimeCompare([]byte(tok), []byte(a.shellToken)) == 1
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// Bound to the app's lifetime, so the ticker stops with the window rather
	// than outliving it.
	a.startUpdateWatch(ctx)
}

// UpdateGate is the force-update decision the app-shell frontend reads to
// render the mandatory "update required" screen. Blocked is true when the
// running build is below Core's advertised min_version.
type UpdateGate struct {
	Blocked    bool   `json:"blocked"`
	Current    string `json:"current"`
	MinVersion string `json:"minVersion"`
}

// GetUpdateGate reports the force-update decision. Bound to the frontend; the
// mandatory screen renders when Blocked is true. The download button reuses the
// Phase-1 OpenUpdateDownload, so no download URL is carried here.
func (a *App) GetUpdateGate() *UpdateGate {
	a.gateMu.Lock()
	defer a.gateMu.Unlock()
	return &UpdateGate{Blocked: a.gateBlocked, Current: AppVersion, MinVersion: a.gateMin}
}

func (a *App) gateIsBlocked() bool {
	a.gateMu.Lock()
	defer a.gateMu.Unlock()
	return a.gateBlocked
}

// gateFloor returns the currently advertised force-update minimum, or "" when no
// gate is active. Read by runUpdate's anti-rollback check so a signed-but-older
// manifest can never satisfy the mandatory floor.
func (a *App) gateFloor() string {
	a.gateMu.Lock()
	defer a.gateMu.Unlock()
	return a.gateMin
}

// setUpdateChannel caches the effective channel Core reported. An empty/unknown
// value is normalized to "stable" so the updater never checks the dev manifest
// on a garbage value.
func (a *App) setUpdateChannel(ch string) {
	if ch != beamChannelDev {
		ch = beamChannelStable
	}
	a.updateChannel.Store(ch)
}

// getUpdateChannel returns the cached effective channel, defaulting to "stable"
// before the first config fetch.
func (a *App) getUpdateChannel() string {
	if v, ok := a.updateChannel.Load().(string); ok && v != "" {
		return v
	}
	return beamChannelStable
}

// GetUpdateChannel is bound to the frontend so App.tsx can render a dev badge
// when the logged-in user is on the dev channel. Read-only, no secret, so it is
// deliberately not shell-token gated.
func (a *App) GetUpdateChannel() string {
	return a.getUpdateChannel()
}

// manifestURLForChannel returns the signed manifest URL for the cached channel:
// the dev (prerelease) manifest on the dev channel, else the stable manifest.
// Both are signed by the same key and verified identically.
func (a *App) manifestURLForChannel() string {
	if a.getUpdateChannel() == beamChannelDev {
		return devManifestURL
	}
	return manifestURL
}

// triggerMandatoryUpdate flips the gate and pulls the webview off the Panel
// onto the app-shell mandatory screen (/__beam/, where App.tsx renders it when
// GetUpdateGate reports blocked). The proxy middleware also redirects Panel
// requests while blocked, so a manual reload can't escape. Safe from any
// goroutine.
func (a *App) triggerMandatoryUpdate(minVer string) {
	a.gateMu.Lock()
	a.gateBlocked = true
	a.gateMin = minVer
	a.gateMu.Unlock()
	if a.ctx != nil {
		runtime.WindowExecJS(a.ctx, "window.location.href='/__beam/'")
	}
}

// evaluateUpdateGate is the PROACTIVE startup gate: once authenticated it reads
// Core's advertised min_version and blocks if this build is below it. Called
// (backgrounded) from SetSession. Best-effort - a fetch error leaves the gate
// unchanged; the reactive 426 in ConnectToServer is the backstop.
func (a *App) evaluateUpdateGate() {
	client := a.getClient()
	if client == nil {
		return
	}
	config, err := client.GetBeamConfig()
	if err != nil || config == nil {
		return
	}
	// Cache the effective update channel so the updater checks the right manifest
	// and the frontend can show a dev badge.
	a.setUpdateChannel(config.UpdateChannel)
	if belowMinVersion(AppVersion, config.MinVersion) {
		a.triggerMandatoryUpdate(config.MinVersion)
	}
}

// ApplyUpdate is the bound self-update entry point (Phase 3). Both the mandatory
// screen and the optional nag call it. It admits exactly one run at a time and
// drives the flow on a background goroutine so the bound call returns to the
// frontend immediately; progress and terminal state reach the UI via the
// update:progress and update:status events.
func (a *App) ApplyUpdate(token string) {
	if !a.checkShellToken(token) {
		return // broker isolation: only the first-party shell may self-update
	}
	if !a.updateInFlight.CompareAndSwap(false, true) {
		return // a run is already in progress
	}
	go func() {
		defer a.updateInFlight.Store(false)
		a.runUpdate()
	}()
}

// runUpdate executes download -> verify -> apply -> relaunch, emitting an
// update:status {state,message} at each transition (states: downloading,
// verifying, applying, relaunching, or error). It RE-VERIFIES fail-closed even
// though the buttons only appear when a verified update exists: the manifest is
// re-fetched and re-checked, and verifyUpdateBinary gates the apply, so a
// poisoned/placeholder path can never replace the running binary.
func (a *App) runUpdate() {
	emitStatus := func(state, message string) {
		if a.ctx == nil {
			return
		}
		runtime.EventsEmit(a.ctx, "update:status", map[string]interface{}{
			"state":   state,
			"message": message,
		})
	}

	m, err := fetchVerifiedManifest(a.manifestURLForChannel())
	if err != nil {
		emitStatus("error", "No verified update is available.")
		return
	}
	// Anti-rollback: the manifest signature proves authenticity, NOT freshness. A
	// validly-signed but older-or-equal manifest served from a compromised
	// CDN/cache must never be downloaded and applied - that is a silent downgrade
	// defeating the mandatory-update control. Require the offered version to be
	// strictly newer than the running build and, when a force-update gate is
	// active, not below its floor, BEFORE any download or apply.
	if !updateIsNewer(m.Version, AppVersion, a.gateFloor()) {
		emitStatus("error", "The offered update is not newer than the installed version; nothing was changed.")
		return
	}
	dlURL, wantSha, sigB64, err := platformArtifact(m)
	if err != nil {
		emitStatus("error", err.Error())
		return
	}

	emitStatus("downloading", "")
	data, err := a.downloadUpdate(dlURL)
	if err != nil {
		emitStatus("error", "Download failed: "+err.Error())
		return
	}

	emitStatus("verifying", "")
	if err := verifyUpdateBinary(updatePublicKeyB64, data, wantSha, sigB64); err != nil {
		emitStatus("error", "Update verification failed; nothing was changed.")
		return
	}

	emitStatus("applying", "")
	if err := applyUpdate(data); err != nil {
		emitStatus("error", err.Error())
		return
	}

	emitStatus("relaunching", "")
	if err := a.relaunch(); err != nil {
		// Applied but the relaunch did not start: surface the restart-manually message.
		emitStatus("error", err.Error())
		return
	}
}

// ─── Connection-state accessors (connMu-guarded) ─────────────────────
// Leaf helpers: each takes connMu only for the single field access and
// never calls another method, so callers can compose them freely without
// risking a deadlock. Close() on a returned relay client is always done by
// the caller OUTSIDE the lock, since teardown can block on network I/O.

func (a *App) getClient() *CoreClient {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return a.client
}

func (a *App) getRelay() *BeamNodeClient {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return a.relayClient
}

func (a *App) setClient(c *CoreClient) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	a.client = c
}

// setConnMode records the transport that won the preference chain for the live tunnel
// ("lan-fastpath", "relay", or "direct"). Read back by the bound GetConnectionMode so
// the FileBrowser can show a connection-mode badge.
func (a *App) setConnMode(mode string) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	a.connMode = mode
}

// GetConnectionMode returns the active transport label for the live tunnel, or ""
// when not connected. Bound to the frontend (read-only): it exposes no secret and has
// no side effect, so it is deliberately NOT shell-token gated (the WS2 gate guards the
// three side-effecting methods, not a passive getter).
func (a *App) GetConnectionMode() string {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return a.connMode
}

// setRelay swaps in a new relay client and returns the previous one for the
// caller to Close outside the lock.
func (a *App) setRelay(rc *BeamNodeClient) *BeamNodeClient {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	old := a.relayClient
	a.relayClient = rc
	return old
}

// newClientResetRelay installs a fresh REST client and drops the cached relay
// (a new session invalidates the old tunnel). Returns the old relay to Close.
func (a *App) newClientResetRelay(c *CoreClient) *BeamNodeClient {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	old := a.relayClient
	a.client = c
	a.relayClient = nil
	a.connMode = ""
	return old
}

// resetSession clears all session + connection state and returns the relay
// client to Close outside the lock.
func (a *App) resetSession() *BeamNodeClient {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	old := a.relayClient
	a.client = nil
	a.relayClient = nil
	a.connMode = ""
	return old
}

// ─── Auth ────────────────────────────────────────────────────────────

// isPanelOrigin reports whether apiURL points at the SAME origin as the
// configured Panel: identical host AND identical scheme. Both Login and
// SetSession are Wails-bound, so a compromised/MITM'd proxied Panel (which
// shares the wails:// origin) could call them with an attacker-controlled URL
// to exfiltrate the JWT-carrying REST client or hijack the session. Pinning to
// the Panel origin blocks that. The scheme is matched against the Panel target's
// OWN scheme rather than hard-coded https, so a legitimate localhost/self-host
// http Panel keeps working while an http:// downgrade of an https Panel (which
// would send the JWT in cleartext to the same host) is rejected.
func (a *App) isPanelOrigin(apiURL string) bool {
	u, err := url.Parse(apiURL)
	if err != nil || u.Host == "" {
		return false
	}
	target := a.resolvePanelTarget()
	return u.Host == target.Host && u.Scheme == target.Scheme
}

// Login authenticates with the Core REST API and returns the session info.
func (a *App) Login(apiURL, username, password string) (*LoginResult, error) {
	// Pin to the Panel origin before creating or installing any client, so a
	// compromised Panel cannot point the credential-bearing REST client at an
	// attacker host (exfil/SSRF) or hijack the session.
	if !a.isPanelOrigin(apiURL) {
		return nil, fmt.Errorf("login: apiURL must match the panel origin")
	}
	client, err := NewCoreClient(apiURL)
	if err != nil {
		return nil, err
	}

	result, err := client.Login(username, password)
	if err != nil {
		return nil, err
	}

	a.setClient(client)
	return result, nil
}

// Logout clears the session.
func (a *App) Logout() {
	a.stopHealthCheck()
	if old := a.resetSession(); old != nil {
		old.Close()
	}
}

// SetSession is the panel-driven counterpart to Login: when the Beam
// Desktop loads the Panel and the user authenticates against Core, the
// Panel JS pushes the resulting JWT into the Wails side so subsequent
// file ops can use the same identity. Idempotent — safe to call on
// every page load.
func (a *App) SetSession(apiURL, token string) error {
	if apiURL == "" || token == "" {
		return fmt.Errorf("apiURL and token are required")
	}
	// The token is a live JWT credential. Only accept an apiURL on the SAME
	// origin (host AND scheme) as the configured Panel so a compromised Panel
	// page can neither push the token to an attacker-controlled host nor
	// downgrade it onto an http:// transport of the Panel host.
	if !a.isPanelOrigin(apiURL) {
		return fmt.Errorf("apiURL does not match the configured Panel origin")
	}
	client, err := NewCoreClient(apiURL)
	if err != nil {
		return err
	}
	client.token = token
	// New session invalidates any cached relay connection — the old
	// tunnel was tied to the previous user's tickets, and the
	// health-check goroutine was watching that tunnel.
	old := a.newClientResetRelay(client)
	a.stopHealthCheck()
	if old != nil {
		old.Close()
	}
	// Proactive force-update gate: now authenticated, check Core's advertised
	// minimum and block if this build is too old. Backgrounded so SetSession
	// returns to the Panel JS promptly (the check is a network round-trip).
	go a.evaluateUpdateGate()
	return nil
}

// stopHealthCheck signals the current health-check goroutine to exit
// (if one is running) and forgets it. Safe to call multiple times.
func (a *App) stopHealthCheck() {
	a.healthCheckMu.Lock()
	defer a.healthCheckMu.Unlock()
	if a.healthCheckStop != nil {
		close(a.healthCheckStop)
		a.healthCheckStop = nil
	}
}

// startHealthCheck (re)starts the background ping loop for the given
// serverUUID. Idempotent — replaces an existing checker (so a switch
// to a different server cleanly stops the old one) and is safe to call
// after every ConnectToServer.
func (a *App) startHealthCheck(serverUUID string) {
	a.healthCheckMu.Lock()
	if a.healthCheckStop != nil {
		close(a.healthCheckStop)
	}
	stop := make(chan struct{})
	a.healthCheckStop = stop
	a.healthCheckMu.Unlock()
	go a.runHealthCheck(serverUUID, stop)
}

// runHealthCheck pings the relay every 30s. On Ping failure the relay
// address cache is cleared and ConnectToServer is invoked again — that
// path re-fetches GetBeamConfig and gets a fresh (potentially different)
// relay from Core's random pick, so a dead relay rotates out without
// the user noticing. We deliberately skip the check while uploads are
// in flight so we never race the JS-side resume retry loop.
func (a *App) runHealthCheck(serverUUID string, stop chan struct{}) {
	const (
		interval    = 30 * time.Second
		pingTimeout = 5 * time.Second
	)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// Don't ping while an upload is mid-flight — the JS side has
			// its own retry/resume path and tearing the tunnel out from
			// under it would just cause needless extra resume attempts.
			a.uploadsMu.Lock()
			inFlight := len(a.uploads)
			a.uploadsMu.Unlock()
			if inFlight > 0 {
				continue
			}
			relay := a.getRelay()
			if relay == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
			err := relay.Ping(ctx)
			cancel()
			if err == nil {
				continue
			}
			// Ping failed - reconnect. ConnectToServer asks Core for the
			// relay address on every call, so there is nothing to invalidate
			// first: this used to clear a cached copy that no reader ever
			// consulted. If Core hands us back the same dead relay (its Redis
			// TTL has not expired yet) we just fail again on the next tick -
			// eventually the dead one ages out.
			if cErr := a.ConnectToServer(serverUUID); cErr != nil {
				// Log to stderr; Wails apps don't have a UI for it but
				// it surfaces when launched from a console / dev mode.
				fmt.Fprintf(os.Stderr, "beam-app: health-check reconnect failed: %v (original ping err: %v)\n", cErr, err)
			}
		}
	}
}

// IsLoggedIn checks if there is an active session.
func (a *App) IsLoggedIn() bool {
	c := a.getClient()
	return c != nil && c.token != ""
}

// ─── Beam Config ─────────────────────────────────────────────────────

// GetBeamConfig returns the Beam relay address and branding from Core.
//
// It used to say it also cached the address "for ConnectToServer", and it did
// store one - but ConnectToServer fetches the config itself on every connect
// and never read the cached copy. The field was written and cleared and never
// consulted, so it is gone along with the claim.
func (a *App) GetBeamConfig() (*BeamConfig, error) {
	client := a.getClient()
	if client == nil {
		return nil, fmt.Errorf("not logged in")
	}
	return client.GetBeamConfig()
}

// ─── Servers ─────────────────────────────────────────────────────────

// GetServers returns the server list for the logged-in user.
func (a *App) GetServers() ([]BeamServer, error) {
	client := a.getClient()
	if client == nil {
		return nil, fmt.Errorf("not logged in")
	}
	return client.GetBeamServers()
}

// ConnectToServer opens a file-ops tunnel to the Node hosting this server, trying
// transports in a preference chain decided by Core's ticket + relay presence:
//  1. LAN fast-path - a pinned-TLS dial to the node's LAN IPs when Core supplied a
//     fingerprint and LAN IPs. Tried FIRST even when a relay is present, so a
//     co-located client gets the fast path.
//  2. Relay - the IP-hiding relay hop, when Core reports a relay address.
//  3. Public-direct - a pinned-TLS dial to the node's public address, only when no
//     relay is present.
//
// Direct dials (1 and 3) only ever use the fingerprint-pinned TLS port, never the
// plain overlay port, so a direct hop is encrypted and MITM-proof or it does not
// happen. The winning transport is recorded (GetConnectionMode) for the UI badge.
//
// GETs (not POSTs): a CDN/WAF in front of Core was handing the desktop app's POSTs an
// HTML challenge page; GET /beam/* goes through cleanly.
func (a *App) ConnectToServer(serverUUID string) error {
	client := a.getClient()
	if client == nil {
		return fmt.Errorf("not logged in")
	}

	ticket, err := client.GetBeamTicket(serverUUID)
	if err != nil {
		// A mid-session raise of beam.min_version surfaces here as a typed 426:
		// show the mandatory screen instead of a generic error.
		var ure *UpdateRequiredError
		if errors.As(err, &ure) {
			a.triggerMandatoryUpdate(ure.MinVersion)
		}
		return fmt.Errorf("ticket: %w", err)
	}

	// Ask Core whether a relay is present. A non-empty address == gateway present.
	config, err := client.GetBeamConfig()
	if err != nil {
		return fmt.Errorf("beam config: %w", err)
	}
	relayAddr := strings.TrimSpace(config.RelayAddress)

	// Preference chain: LAN fast-path first (even with a relay), then the IP-hiding
	// relay, then a public-direct dial. The pure beamConnectPlan decides the order and
	// gating; this loop performs the actual dials and records the winning transport.
	plan := beamConnectPlan(ticket.LANFingerprint != "", ticket.LANIPs, relayAddr, ticket.PublicAddr)
	var (
		rc      *BeamNodeClient
		mode    string
		lastErr error
	)
	for _, step := range plan {
		switch step {
		case "lan":
			if c, ok := a.dialLANFastpath(ticket); ok {
				rc, mode = c, "lan-fastpath"
			}
		case "relay":
			// ConnectBeamNode already returns a self-descriptive English message.
			if c, derr := ConnectBeamNode(relayAddr, ticket.Ticket); derr != nil {
				lastErr = derr
			} else {
				rc, mode = c, "relay"
			}
		case "public":
			if c, derr := a.dialPublicDirect(ticket); derr != nil {
				lastErr = derr
			} else {
				rc, mode = c, "direct"
			}
		}
		if rc != nil {
			break
		}
	}

	if rc == nil {
		// A failed switch must not leave the previous server's tunnel and badge
		// installed - they would misrepresent the now-selected server. Tear the stale
		// connection down so file ops fall back to Core REST for the correct server and
		// the connection-mode badge clears.
		a.stopHealthCheck()
		if old := a.setRelay(nil); old != nil {
			old.Close()
		}
		a.setConnMode("")

		if lastErr != nil {
			return fmt.Errorf("could not reach the server over LAN, relay, or a direct connection: %w", lastErr)
		}
		// Nothing was dialable: no relay and no pinnable direct target.
		port := ticket.LANPort
		if port == "" {
			port = "25523"
		}
		return fmt.Errorf("node not reachable: no gateway/relay is configured and Core provided no pinned-TLS direct target - open the node's beam port (%s) so this client can reach it, or add a gateway/relay to route the connection", port)
	}

	a.setConnMode(mode)
	if old := a.setRelay(rc); old != nil {
		old.Close()
	}
	a.startHealthCheck(serverUUID)
	return nil
}

// dialLANFastpath probes the ticket's LAN IPs and, on the first reachable one, opens a
// pinned-TLS dial to the node's LAN fast-path port. Returns (client, true) on success,
// (nil, false) otherwise. It never attempts the public address - the chain in
// ConnectToServer decides what to try next. Requires a pinned fingerprint (the plan
// only schedules "lan" when one exists), so it never dials an unpinned target.
func (a *App) dialLANFastpath(ticket *BeamTicket) (*BeamNodeClient, bool) {
	if ticket.LANFingerprint == "" || len(ticket.LANIPs) == 0 {
		return nil, false
	}
	// Client-side backstop: drop any LAN hint that is not an RFC1918/link-local LAN
	// address before probing. Core already hard-filters these, but a poisoned ticket
	// must never make the app dial an off-LAN (deanonymizing) target.
	lanIPs := filterPrivateLANIPs(ticket.LANIPs)
	if len(lanIPs) == 0 {
		return nil, false
	}
	port := ticket.LANPort
	if port == "" {
		port = "25523" // pinned-TLS beam port (BEAM_LAN_PORT default)
	}
	addr := probeBeamLAN(lanIPs, port)
	if addr == "" {
		return nil, false
	}
	rc, derr := ConnectBeamNodeDirect(addr, ticket.Ticket, ticket.LANFingerprint)
	if derr != nil {
		fmt.Fprintf(os.Stderr, "beam-app: direct LAN dial to %s failed: %v\n", addr, derr)
		return nil, false
	}
	return rc, true
}

// dialPublicDirect dials the node's public address on its pinned-TLS beam port. Used
// as the no-relay fallback. Refuses when Core supplied no fingerprint (never an
// unpinned direct dial) or no public address.
func (a *App) dialPublicDirect(ticket *BeamTicket) (*BeamNodeClient, error) {
	if ticket.LANFingerprint == "" {
		return nil, fmt.Errorf("node not reachable directly: Core provided no pinned-TLS fingerprint, and no gateway/relay is configured - add a gateway/relay to route the connection")
	}
	if ticket.PublicAddr == "" {
		return nil, fmt.Errorf("node not reachable directly: Core provided no public address, and no gateway/relay is configured - open the node's beam port or add a gateway/relay")
	}
	rc, derr := ConnectBeamNodeDirect(ticket.PublicAddr, ticket.Ticket, ticket.LANFingerprint)
	if derr != nil {
		fmt.Fprintf(os.Stderr, "beam-app: direct public dial to %s failed: %v\n", ticket.PublicAddr, derr)
		return nil, derr
	}
	return rc, nil
}

// ─── File Operations ─────────────────────────────────────────────────
// Prefer relay (gRPC via BeamRelay) when available; fall back to Core REST.

func (a *App) ListFiles(path string, serverUUID string) (*FileListResult, error) {
	client := a.getClient()
	if client == nil {
		return nil, fmt.Errorf("not logged in")
	}
	if relay := a.getRelay(); relay != nil {
		return relay.ListFiles(path)
	}
	return client.ListFiles(path, serverUUID)
}

func (a *App) GetFileContent(path string, serverUUID string) (*FileContentResult, error) {
	client := a.getClient()
	if client == nil {
		return nil, fmt.Errorf("not logged in")
	}
	if relay := a.getRelay(); relay != nil {
		return relay.GetFileContent(path)
	}
	return client.GetFileContent(path, serverUUID)
}

func (a *App) SaveFile(path string, content string, serverUUID string) error {
	client := a.getClient()
	if client == nil {
		return fmt.Errorf("not logged in")
	}
	if relay := a.getRelay(); relay != nil {
		return relay.SaveFile(path, content)
	}
	return client.SaveFile(path, content, serverUUID)
}

func (a *App) CreateFile(path string, isDir bool, serverUUID string) error {
	client := a.getClient()
	if client == nil {
		return fmt.Errorf("not logged in")
	}
	if relay := a.getRelay(); relay != nil {
		return relay.CreateFile(path, isDir)
	}
	return client.CreateFile(path, isDir, serverUUID)
}

func (a *App) DeleteFile(path string, serverUUID string) error {
	client := a.getClient()
	if client == nil {
		return fmt.Errorf("not logged in")
	}
	if relay := a.getRelay(); relay != nil {
		return relay.DeleteFile(path)
	}
	return client.DeleteFile(path, serverUUID)
}

func (a *App) RenameFile(oldPath string, newPath string, serverUUID string) error {
	client := a.getClient()
	if client == nil {
		return fmt.Errorf("not logged in")
	}
	if relay := a.getRelay(); relay != nil {
		// BeamNodeService takes just the new filename, not the full path
		return relay.RenameFile(oldPath, filepath.Base(newPath))
	}
	return client.RenameFile(oldPath, newPath, serverUUID)
}

func (a *App) CopyFile(srcPath string, dstPath string, serverUUID string) error {
	client := a.getClient()
	if client == nil {
		return fmt.Errorf("not logged in")
	}
	if relay := a.getRelay(); relay != nil {
		return relay.CopyFile(srcPath, dstPath)
	}
	return client.CopyFile(srcPath, dstPath, serverUUID)
}

// ─── Downloads ───────────────────────────────────────────────────────

// DownloadFile downloads a single file (or zipped directory) from the server.
// Opens a native save dialog so the user picks where to save it.
func (a *App) DownloadFile(path string, serverUUID string, isDir bool) error {
	client := a.getClient()
	if client == nil {
		return fmt.Errorf("not logged in")
	}

	suggestedName := filepath.Base(path)
	if isDir {
		suggestedName += ".zip"
	}

	savePath, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "Save File",
		DefaultFilename: suggestedName,
	})
	if err != nil || savePath == "" {
		return nil // user cancelled
	}

	if relay := a.getRelay(); relay != nil {
		if err := relay.DownloadFile(path, isDir, savePath, func(loaded, total int64) {
			runtime.EventsEmit(a.ctx, "download:progress", map[string]interface{}{
				"loaded": loaded,
				"total":  total,
			})
		}); err != nil {
			return err
		}
		a.setLastDownloadPath(savePath)
		return nil
	}

	rc, err := client.DownloadFile(path, serverUUID)
	if err != nil {
		return err
	}
	defer rc.Close()

	f, err := os.Create(savePath)
	if err != nil {
		return fmt.Errorf("could not create file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, rc); err != nil {
		return err
	}
	a.setLastDownloadPath(savePath)
	return nil
}

// SelectiveDownload downloads selected files/folders as a zip.
// Opens a native save dialog so the user picks where to save it.
func (a *App) SelectiveDownload(basePath string, selected []string, selectAll bool, serverUUID string) error {
	client := a.getClient()
	if client == nil {
		return fmt.Errorf("not logged in")
	}

	savePath, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "Save Archive",
		DefaultFilename: "download.zip",
	})
	if err != nil || savePath == "" {
		return nil // user cancelled
	}

	if relay := a.getRelay(); relay != nil {
		if err := relay.SelectiveDownload(basePath, selected, selectAll, savePath, func(loaded, total int64) {
			runtime.EventsEmit(a.ctx, "download:progress", map[string]interface{}{
				"loaded": loaded,
				"total":  total,
			})
		}); err != nil {
			return err
		}
		a.setLastDownloadPath(savePath)
		return nil
	}

	rc, err := client.SelectiveDownload(basePath, selected, selectAll, serverUUID)
	if err != nil {
		return err
	}
	defer rc.Close()

	f, err := os.Create(savePath)
	if err != nil {
		return fmt.Errorf("could not create file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, rc); err != nil {
		return err
	}
	a.setLastDownloadPath(savePath)
	return nil
}

// ─── Chunked Uploads (Beam-native, multi-GB safe) ────────────────────
// Three-call protocol driven by JS: Start → many Chunk → Finish (or
// Cancel). Each call references an opaque uploadID minted by the JS
// side so multiple uploads can't accidentally share a stream. The
// underlying gRPC stream stays open between calls, so chunks land
// straight on the Node's disk without buffering on Core or Relay.
// Chunks arrive base64-encoded because Wails IPC serializes everything
// as JSON; the per-chunk inflate (33%) is irrelevant compared to the
// HTTP-multipart base64-ish wrap it replaces.

func (a *App) BeamUploadStart(uploadID, path, filename, strategy string, totalSize int64) error {
	relay := a.getRelay()
	if relay == nil {
		return fmt.Errorf("no relay connection (call ConnectToServer first)")
	}
	if uploadID == "" {
		return fmt.Errorf("uploadID is required")
	}
	sess, err := relay.UploadStart(uploadID, path, filename, strategy, totalSize)
	if err != nil {
		return err
	}
	a.uploadsMu.Lock()
	// Overwriting a stale ID would orphan the previous stream — abort
	// it explicitly so we don't leak goroutines on the Node side.
	if prev := a.uploads[uploadID]; prev != nil {
		prev.Cancel()
	}
	a.uploads[uploadID] = sess
	a.uploadsMu.Unlock()
	return nil
}

// BeamUploadResume is the JS-driven retry path. JS calls it after a
// chunk Send fails (relay restart, transient network blip), and it
//  1. forces a fresh ConnectToServer — picks up a new relay address
//     from Core / re-handshakes the JWT ticket
//  2. opens a new UploadFile stream with the SAME uploadID
//     (gRPC metadata "x-beam-upload-id") so the Node reattaches to
//     the existing temp file at its current size
//
// JS then continues WriteChunk from the offset it had reached before
// the failure. The Node's WriteAt is idempotent on identical offsets,
// so a re-sent partial chunk is harmless.
func (a *App) BeamUploadResume(uploadID, serverUUID, path, filename, strategy string, totalSize int64) error {
	if a.getClient() == nil {
		return fmt.Errorf("not logged in")
	}
	if uploadID == "" {
		return fmt.Errorf("uploadID is required")
	}
	// Tear down the dead session before opening a new one so the
	// goroutine on the old stream doesn't outlive its tunnel.
	a.uploadsMu.Lock()
	if prev := a.uploads[uploadID]; prev != nil {
		prev.Cancel()
		delete(a.uploads, uploadID)
	}
	a.uploadsMu.Unlock()
	// Force a fresh ConnectToServer — if the relay died, the cached
	// relayClient is dead too and would just fail again.
	if err := a.ConnectToServer(serverUUID); err != nil {
		return fmt.Errorf("reconnect: %w", err)
	}
	relay := a.getRelay()
	if relay == nil {
		return fmt.Errorf("resume open: no relay connection after reconnect")
	}
	sess, err := relay.UploadStart(uploadID, path, filename, strategy, totalSize)
	if err != nil {
		return fmt.Errorf("resume open: %w", err)
	}
	a.uploadsMu.Lock()
	a.uploads[uploadID] = sess
	a.uploadsMu.Unlock()
	return nil
}

func (a *App) BeamUploadChunk(uploadID, dataB64 string, offset int64) error {
	a.uploadsMu.Lock()
	sess := a.uploads[uploadID]
	a.uploadsMu.Unlock()
	if sess == nil {
		return fmt.Errorf("unknown upload id")
	}
	data, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return fmt.Errorf("decode chunk: %w", err)
	}
	return sess.WriteChunk(data, offset)
}

func (a *App) BeamUploadFinish(uploadID string) error {
	a.uploadsMu.Lock()
	sess := a.uploads[uploadID]
	delete(a.uploads, uploadID)
	a.uploadsMu.Unlock()
	if sess == nil {
		return fmt.Errorf("unknown upload id")
	}
	return sess.Finish()
}

// BeamUploadCancel is fire-and-forget — JS uses it as a best-effort
// cleanup when an upload aborts mid-stream (network blip, user cancel).
// Returning an error here just clutters the FileBrowser error path.
func (a *App) BeamUploadCancel(uploadID string) {
	a.uploadsMu.Lock()
	sess := a.uploads[uploadID]
	delete(a.uploads, uploadID)
	a.uploadsMu.Unlock()
	if sess != nil {
		sess.Cancel()
	}
}

// ─── Native-only extensions (Beam-specific) ──────────────────────────

// setLastDownloadPath records the destination of a just-completed
// DownloadFile/SelectiveDownload so RevealInExplorer has something
// trustworthy to open. Never derived from a JS-supplied argument.
func (a *App) setLastDownloadPath(path string) {
	a.downloadMu.Lock()
	a.lastDownloadPath = path
	a.downloadMu.Unlock()
}

// RevealInExplorer opens the platform's native file manager pointed at
// the most recently downloaded file. Used by the Panel for the "Reveal
// in Explorer / Finder" button which only renders inside the Beam
// Desktop App (the browser version has no equivalent).
//
// Deliberately no-arg (BC3 twin): this used to take a caller-supplied
// localPath and hand it straight to the OS shell-open, but it is a
// Wails-bound method reachable as window.go.main.App.RevealInExplorer(...)
// from ANY JS in the webview - the Wails webview reverse-proxies the
// remote Panel onto the wails:// origin, so a compromised/MITM'd Panel
// (or a user-set malicious Panel URL) could call it with a file://, UNC,
// or other dangerous path, triggering a native OS shell-open. The only
// legitimate target is wherever the user's own native save dialog
// (DownloadFile/SelectiveDownload) just wrote the file - not an
// arbitrary path the frontend hands us - so we track that path
// server-side instead of trusting an argument.
func (a *App) RevealInExplorer() error {
	a.downloadMu.Lock()
	path := a.lastDownloadPath
	a.downloadMu.Unlock()
	if path == "" {
		return fmt.Errorf("no downloaded file to reveal")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("downloaded file no longer exists: %w", err)
	}
	// The CONTAINING FOLDER, not the file.
	//
	// BrowserOpenURL is a browser-open, not a file-manager reveal: on Windows it
	// goes through url.dll's FileProtocolHandler, on macOS through `open`, on
	// Linux through xdg-open. Handed a file:// URL pointing at a FILE, all three
	// launch that file in its default handler - so this button, labelled
	// "reveal", would hand a downloaded .jar to javaw and a .zip to the
	// archiver. It never selected the file the way the old comment here claimed;
	// that needs `explorer /select,` and its per-OS twins. Pointing at the
	// directory gives the honest behaviour on all three platforms in one line:
	// the folder opens, and nothing that came off the server is executed.
	runtime.BrowserOpenURL(a.ctx, fileURL(filepath.Dir(abs)))
	return nil
}

// fileURL renders a local path as a well-formed file:// URL.
//
// Concatenating "file://" with the path is wrong on Windows: "C:/x" yields
// file://C:/x, where "C:" parses as the URL's HOST, and it leaves spaces and
// other reserved characters unescaped. Prefixing the drive-letter path with "/"
// and letting url.URL do the escaping produces file:///C:/x and percent-encodes
// the rest. On Unix the path already starts with "/", so the prefix is a no-op.
func fileURL(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// panelCookies returns the shell's cookie jar, creating it on first use.
//
// It holds the panel session so the credential never enters the webview; see
// applyShellCookies in proxy.go for why Beam cannot rely on the embedded
// browser to store it. Lazily built because cookiejar.New can technically fail
// and a constructor that can fail is not worth threading through NewApp for
// something every path can recreate.
func (a *App) panelCookies() http.CookieJar {
	a.cookieMu.Lock()
	defer a.cookieMu.Unlock()
	if a.cookies == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			// Documented as always-nil for a nil options value. A jar that
			// drops everything is still better than a nil interface panicking
			// inside the proxy.
			jar, _ = cookiejar.New(&cookiejar.Options{})
		}
		// Refill from disk before anyone reads it. Done here rather than in
		// main because this is the only place a jar comes into existence -
		// forgetPanelSession drops it and the next request builds another, so a
		// one-shot restore at startup would be skipped on exactly that path.
		stored := readStoredSessions()
		restoreInto(jar, stored)
		a.sessionMu.Lock()
		a.sessionFingerprint = stored.fingerprint()
		a.sessionMu.Unlock()
		a.cookies = jar
	}
	return a.cookies
}

// forgetPanelSession drops the shell's cookies.
//
// This is the only way to end a Beam session locally: the webview never held
// the credential, so clearing site data there reaches nothing. Used by the
// settings page's "clear local data" action and whenever the app is pointed at
// a different panel - carrying one panel's session to another would at best
// fail and at worst send it somewhere it does not belong.
func (a *App) forgetPanelSession() {
	a.cookieMu.Lock()
	a.cookies = nil
	a.cookieMu.Unlock()
	// Take it off disk too. Dropping only the in-memory jar would sign the user
	// out until the next launch, which is not what "clear local data" means and
	// leaves the token sitting in a file after they asked for it to be gone.
	a.clearStoredSession()
	// And the readable half, which is a SECOND record of the same fact. The
	// shell's jar is what authenticates a proxied request; the readable set is
	// what the injected script writes into document.cookie so the panel can see
	// it is signed in. Clearing only the first leaves the next page load
	// re-injecting the sign-in hint over a session that no longer exists - the
	// panel then renders its authed shell and every request inside it fails,
	// which is worse than being signed out, and is not what the user asked for.
	a.readableMu.Lock()
	a.readable = nil
	a.readableMu.Unlock()
}

// The panel list, exposed to the app-shell settings page.
//
// GetPanelURL and GetAPIURL now answer for the ACTIVE entry, so every existing
// caller - the proxy target, the CSP connect-src, the deploy-kit's coreOrigin -
// follows a switch without knowing the list exists.

// ListPanels returns the saved panels and which one is active.
func (a *App) ListPanels() map[string]interface{} {
	s := loadSettings()
	list := s.panelList()
	out := make([]map[string]interface{}, 0, len(list))
	for _, p := range list {
		out = append(out, map[string]interface{}{
			"name":    p.DisplayName(),
			"rawName": p.Name,
			"url":     p.URL,
			"apiUrl":  p.APIURL,
			// Reported rather than inferred client-side: the built-in entry moves
			// with DYLARIS_PANEL_URL, so its URL is not a constant the UI knows.
			"official": p.IsOfficial(),
		})
	}
	return map[string]interface{}{"panels": out, "active": s.activePanel().URL}
}

// SavePanels replaces the whole list and the active choice in one write.
//
// One call rather than add/remove/rename, because the settings page edits the
// list as a whole and a partial API would let the two disagree about which entry
// is active mid-edit. Shell-token gated like every other side-effecting binding:
// a compromised proxied panel must not be able to add an entry and point the app
// at it.
func (a *App) SavePanels(token string, panels []savedPanel, active string) error {
	if !a.checkShellToken(token) {
		return fmt.Errorf("unauthorized")
	}
	cleaned := make([]savedPanel, 0, len(panels))
	seen := map[string]bool{}
	for _, p := range panels {
		u, err := normalisePanelURL(p.URL)
		if err != nil {
			return err
		}
		if seen[strings.ToLower(u)] {
			// Two entries for one URL would share a session and a jar while
			// looking like separate places to be signed in.
			continue
		}
		seen[strings.ToLower(u)] = true
		api := strings.TrimRight(strings.TrimSpace(p.APIURL), "/")
		if api != "" {
			if _, err := normalisePanelURL(api); err != nil {
				return err
			}
		}
		cleaned = append(cleaned, savedPanel{Name: strings.TrimSpace(p.Name), URL: u, APIURL: api})
	}
	// Empty is fine when this build HAS a built-in entry: panelList puts it back,
	// so the list is never actually empty and refusing here would only stop
	// someone from removing the last panel they added. With no built-in entry
	// (the open-source build) an empty list really would leave the app with
	// nowhere to go, and the refusal still stands.
	if len(cleaned) == 0 && officialPanel().URL == "" {
		return fmt.Errorf("keep at least one panel")
	}

	s := loadSettings()
	s.Panels = cleaned
	s.Active = strings.TrimRight(strings.TrimSpace(active), "/")
	// The legacy single field is cleared once a list exists, so a future read
	// cannot resurrect a panel that was removed here.
	s.PanelURL, s.APIURL = "", ""
	return saveSettings(s)
}

// SwitchPanel points the window at one of the saved panels.
//
// It does NOT clear any session. The shell holds one jar per host, so the
// session for the panel being left stays valid and switching back is instant -
// which is the whole reason the list is worth having rather than an edit box.
func (a *App) SwitchPanel(token, panelURL string) error {
	if !a.checkShellToken(token) {
		return fmt.Errorf("unauthorized")
	}
	target := strings.TrimRight(strings.TrimSpace(panelURL), "/")
	s := loadSettings()
	for _, p := range s.panelList() {
		if p.URL == target {
			s.Active = target
			return saveSettings(s)
		}
	}
	return fmt.Errorf("that panel is not in the list")
}
