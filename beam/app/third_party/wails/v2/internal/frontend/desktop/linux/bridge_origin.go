//go:build linux
// +build linux

package linux

// DYLARIS PATCH (beam): the native bridge answers only the app's own page.
//
// WebKitGTK exposes window.webkit.messageHandlers.external to every document
// in the window: a cross-origin iframe (the panel frames tenant tab-proxy
// pages), an about:blank child that iframe creates, and a foreign page the
// main frame is navigated to (a redirect the panel proxy passes through, a
// dropped file). Measured on WebKitGTK 2.50 (4.0 and 4.1 APIs): all three
// reached sendMessageToBackend, and the script-message-received signal says
// nothing about which frame sent a message. processMessage dispatched every
// one of them - any bound method, with the session the Go side holds.
//
// So the app's page proves itself instead. A user script injected into the
// TOP frame only, at document start (before any page script), and only when
// that document is on the start URL's origin, wraps postMessage in that
// realm to prefix a per-run secret. Frames have their own realm and get no
// script; a foreign top document gets the script but it returns before
// touching anything. processMessage drops whatever lacks the prefix.

/*
#cgo linux pkg-config: gtk+-3.0
#cgo !webkit2_41 pkg-config: webkit2gtk-4.0
#cgo webkit2_41 pkg-config: webkit2gtk-4.1

#include <stdlib.h>
#include <webkit2/webkit2.h>

static void addTopFrameStartScript(void *contentManager, const char *source)
{
    WebKitUserScript *script = webkit_user_script_new(source,
        WEBKIT_USER_CONTENT_INJECT_TOP_FRAME,
        WEBKIT_USER_SCRIPT_INJECT_AT_DOCUMENT_START,
        NULL, NULL);
    webkit_user_content_manager_add_script((WebKitUserContentManager *)contentManager, script);
    webkit_user_script_unref(script);
}
*/
import "C"

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"unsafe"
)

// bridgeToken prefixes every message the app's own page sends.
var bridgeToken = func() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("bridge token: " + err.Error())
	}
	return "dylaris-bridge-" + hex.EncodeToString(b) + "|"
}()

// bridgeGuardScript is the user script that marks the app's own page.
func bridgeGuardScript(app *url.URL) string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return `(function(){
if (location.protocol !== ` + q(app.Scheme+":") + ` || location.host !== ` + q(app.Host) + `) return;
var h = window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.external;
if (!h) return;
var p = Object.getPrototypeOf(h), post = p.postMessage, t = ` + q(bridgeToken) + `;
p.postMessage = function(m){ return post.call(this, t + m); };
})();`
}

// installBridgeGuard adds the script to the window's content manager.
func (w *Window) installBridgeGuard(app *url.URL) {
	src := C.CString(bridgeGuardScript(app))
	defer C.free(unsafe.Pointer(src))
	C.addTopFrameStartScript(w.contentManager, src)
}

// fromAppPage strips the app page's prefix; false means the message came from
// anywhere else and must be dropped.
func fromAppPage(message string) (string, bool) {
	if len(message) < len(bridgeToken) || message[:len(bridgeToken)] != bridgeToken {
		return "", false
	}
	return message[len(bridgeToken):], true
}
