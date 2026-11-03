package main

import (
	"embed"
	"net/http"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

// The embedded frontend bundle is now just the Panel-URL settings page,
// reachable at /__beam/. The actual UI is the Dylaris Panel, reverse-
// proxied through the Wails asset server (see proxy.go) so the webview
// stays on the wails:// origin and Wails keeps injecting its runtime —
// that's what makes window.go.main.App.* available to the Panel.
//
// There is no native menu or splash: the window loads the Panel
// directly. The settings page is reached as a fallback when the Panel
// can't be loaded (the proxy's error page links to it).
//
//go:embed all:frontend/dist
var assets embed.FS

// defaultPanelURL / defaultAPIURL are the Panel (frontend) and Core API (backend)
// addresses a fresh install starts on. They ship as the official Dylaris hosts so
// the stock binary "just works", but are BUILD-CONFIGURABLE so a fork can ship its
// own branded defaults to its users WITHOUT touching source: pass
//
//	wails build -ldflags "-X main.defaultPanelURL=https://panel.acme.com \
//	                      -X main.defaultAPIURL=https://api.acme.com"
//
// (hence vars, not consts), or set the DYLARIS_PANEL_URL / DYLARIS_API_URL env
// vars at launch. Per-install overrides are saved to config.json via the
// Settings page.
//
// defaultAPIURL is EMPTY, and that is the answer for every current deployment:
// Core serves the panel and the API together, so the API is on the panel's own
// origin and 'self' already covers it in the CSP. It used to name
// api.dylaris.com, from when those were two hosts. A value here only widens the
// proxied Panel's connect-src, so a wrong one is not an error you would see -
// it is a permission nobody needs.
var (
	defaultPanelURL = "https://panel.dylaris.com"
	defaultAPIURL   = ""
)

func main() {
	app := NewApp()
	panelURL := os.Getenv("DYLARIS_PANEL_URL")
	if panelURL == "" {
		panelURL = defaultPanelURL
	}
	app.panelURL = panelURL
	apiURL := os.Getenv("DYLARIS_API_URL")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	app.apiURL = apiURL
	// The built-in list entry follows the same two values, so pointing this app
	// somewhere else with DYLARIS_PANEL_URL moves the entry too rather than
	// leaving an unremovable row for a panel this build is not for.
	setBuiltInDefaults(panelURL, apiURL)

	err := wails.Run(&options.App{
		Title:  "Dylaris Beam",
		Width:  1400,
		Height: 900,
		// The panel's own width floor (--app-min-width in globals.css): below
		// it the page scrolls sideways. 1024x576 is small enough to tile the
		// window into a screen corner and still leaves the console usable.
		MinWidth:  1024,
		MinHeight: 576,
		AssetServer: &assetserver.Options{
			Assets: assets,
			// Turns the window into a transparent shell around the
			// remote Panel — see newPanelMiddleware for the why.
			Middleware: func(next http.Handler) http.Handler {
				return newPanelMiddleware(app, next)
			},
		},
		Windows: &windows.Options{
			// Show the branded titlebar icon. Windows-window-chrome only; does
			// not touch the BC3-patched webview dispatcher or asset server.
			DisableWindowIcon: false,
			// A fixed profile folder. WebView2 otherwise names it after the
			// executable, and a downloaded update arrives as "DylarisBeam(2).exe":
			// every rename started an empty profile, without the signed-in hint,
			// and the panel showed its login form to a user whose session the
			// app still held.
			WebviewUserDataPath: webviewDataPath(),
		},
		OnStartup: app.startup,
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
