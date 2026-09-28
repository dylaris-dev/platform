# Vendored dependencies

## `third_party/wails/v2` - vendored + patched Wails v2.10.1

`go.mod` pins Wails to the local tree via
`replace github.com/wailsapp/wails/v2 => ./third_party/wails/v2`.

This is upstream Wails v2.10.1 with ONE security patch (the "BC3" fix): the native
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
