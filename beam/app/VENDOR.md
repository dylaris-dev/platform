# Vendored dependencies

## `third_party/wails/v2` - vendored + patched Wails v2.10.1

`go.mod` pins Wails to the local tree via
`replace github.com/wailsapp/wails/v2 => ./third_party/wails/v2`.

This is upstream Wails v2.10.1 with three security patches. The first, and the
reason the tree is vendored at all, is the "BC3" fix: the native
dispatcher's `processBrowserMessage`
(`third_party/wails/v2/internal/frontend/dispatcher/browser.go`) enforces a
scheme allowlist on the `BrowserOpenURL` bridge call. Upstream Wails only guarded
`window.runtime.BrowserOpenURL` in JavaScript, which page script could bypass with
`window.WailsInvoke("BO:<url>")` to reach `RevealInExplorer` / shell-open with an
attacker-controlled string. Since the beam window reverse-proxies the untrusted
remote Panel at the same origin as the full native bridge, that JS-only guard was
not a real boundary. The patch moves enforcement into the native dispatcher, the
only seam page script cannot reach.

Consequences:
- The tree is committed (not a module-cache fetch) because the patch is not upstream.
- It must NOT be silently bumped to a stock Wails release - a plain `go get -u` would
  drop the patch and reopen the RCE class. To move to a newer Wails, re-apply the
  dispatcher patch on top and re-vendor.
- `vendor_bc3_patch_test.go` in this module asserts the guard through the syntax
  tree of that file: every native open in `processBrowserMessage` must sit under a
  case listing exactly http, https and mailto. It reads the source because the
  vendored tree is its own module (not in the CI matrix) and the package is under
  `internal/`, so there is no seam to call. Re-applying the patch after a bump
  means making that test pass, not just editing the file.
- It is built via `wails build` with `GOWORK` honoring this module's own `replace`
  (the module is a member of the repo `go.work`, and module-level replaces are
  applied for workspace members).

## The bridge origin and permission patches (Windows frontend)

Two more, both in `internal/frontend/desktop/windows/`, marked `DYLARIS PATCH (beam)`:

- **The native bridge answers only the app's own page.** `processMessage` and
  `processMessageWithAdditionalObjects` first call `fromAppOrigin`
  (`bridge_origin.go`), which reads the main document's URL
  (`ICoreWebView2::get_Source`, vtable slot 4 - go-webview2 v1.0.19 passes the
  message callback the text alone) and drops the message unless it is on the
  start URL's origin. Upstream dispatched any page's `chrome.webview.postMessage`,
  and nothing keeps the window on the app's origin: a redirect the panel proxy
  passes through, or a file dropped onto the window, navigates it, and the page
  found there could call every bound method with the session the Go side holds.
- **No permission is granted unasked.** `SetGlobalPermission` is `Deny`, not
  `Allow`: upstream gave every page camera, microphone, location and clipboard
  READ without a prompt. The panel only writes to the clipboard, which needs no
  permission.

`vendor_bridge_patch_test.go` holds both through the syntax tree, like the BC3
test.

Known residual: `processRequest` serves `http://wails.localhost/...` to any
page, so a foreign page in the window can still send a request through the
panel proxy with the session. Core refuses its writes (the proxy forwards the
foreign Origin and Core's same-origin check rejects it) and CORS hides the
answers to its reads; a GET that changes state would not be stopped. Moving to go-webview2 >= 1.0.22 would let the check use the message's own
source (`ICoreWebView2WebMessageReceivedEventArgs.GetSource`) instead.

## External-open protection is a single, platform-agnostic path

Beam's defense against a compromised/MITM'd Panel triggering a native OS
shell-open is ONE shared path, not a per-OS fork, so every frontend (WebView2 on
Windows, WebKitGTK on Linux) inherits it unchanged:

- BC3 scheme allowlist in the native dispatcher
  (`third_party/wails/v2/internal/frontend/dispatcher/browser.go`, reached via
  `processBrowserMessage` in `dispatcher.go`): the `BrowserOpenURL` / `BO:` bridge
  accepts only `http`, `https`, `mailto` and drops `file://`, UNC, `javascript:`.
- WS2 shell-token gate in `app.go` (`checkShellToken`) on the side-effecting bound
  methods (`SavePanelURL`, `ApplyUpdate`, `OpenUpdateDownload`), plus the
  `http`/`https` re-check on the manifest DownloadURL in `updater.go`
  (`isBrowserOpenableURL`).
- The `Sec-Fetch-Dest` navigation gate in `proxy.go` (`serveBeamIndex`) that keeps
  the per-run shell token off same-origin fetch/XHR reads.

There is intentionally NO WebView2 `NewWindowRequested` (or WebKitGTK new-window)
handler to add: Wails v2.10.1 registers none, and a grep for `NewWindowRequested`
across the vendored tree returns zero matches. `window.open` does not open a new
native OS window that could escape these gates, so the Windows/amd64 build (WS6)
needs no platform-specific new-window code.
