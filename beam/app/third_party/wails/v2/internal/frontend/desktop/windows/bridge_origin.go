//go:build windows
// +build windows

package windows

// DYLARIS PATCH (beam): the native bridge answers only the app's own page.
//
// WebMessageReceived delivers chrome.webview.postMessage from whatever
// document the main frame holds, and processMessage dispatched every one of
// them: a bound method call, an event, a window command. Nothing kept the
// window on the app's origin either - a redirect to a foreign host the
// panel proxy passes through, or a file dropped onto the window, navigates
// it - and the page found there could call every bound method with the
// session the Go side holds, and read the answers by defining
// window.wails.Callback itself.
//
// go-webview2 v1.0.19 hands the message callback the text alone, so the
// origin is taken from the main document's own URL (ICoreWebView2::get_Source,
// vtable slot 4 of the stable COM ABI: QueryInterface, AddRef, Release,
// get_Settings, get_Source). A message from a frame never reaches this
// callback; frames post to CoreWebView2Frame, which nothing here handles.

import (
	"errors"
	"net/url"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// getSourceSlot is ICoreWebView2::get_Source in the interface's vtable.
const getSourceSlot = 4

// mainDocumentSource is the URL of the document the main frame shows now.
func (f *Frontend) mainDocumentSource() (string, error) {
	if f.chromium == nil {
		return "", errors.New("no webview")
	}
	ctrl := f.chromium.GetController()
	if ctrl == nil {
		return "", errors.New("no webview controller")
	}
	wv, err := ctrl.GetCoreWebView2()
	if err != nil || wv == nil {
		return "", errors.New("no core webview")
	}
	defer wv.Release()
	vtbl := *(**[getSourceSlot + 1]uintptr)(unsafe.Pointer(wv))
	var src *uint16
	hr, _, _ := syscall.SyscallN(vtbl[getSourceSlot], uintptr(unsafe.Pointer(wv)), uintptr(unsafe.Pointer(&src)))
	if windows.Handle(hr) != windows.S_OK {
		return "", windows.Errno(hr)
	}
	s := windows.UTF16PtrToString(src)
	windows.CoTaskMemFree(unsafe.Pointer(src))
	return s, nil
}

// fromAppOrigin reports whether a bridge message may be acted on: the main
// document must be on the app's own origin. Unknown is refused.
func (f *Frontend) fromAppOrigin() bool {
	src, err := f.mainDocumentSource()
	if err != nil {
		f.logger.Error("bridge: could not read the page's origin, message dropped: " + err.Error())
		return false
	}
	if !sameOrigin(src, f.startURL) {
		f.logger.Warning("bridge: message from a page outside the app dropped")
		return false
	}
	return true
}

// sameOrigin compares scheme and host (with port) of a URL to the app's.
func sameOrigin(raw string, app *url.URL) bool {
	if app == nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == app.Scheme && u.Host == app.Host
}
