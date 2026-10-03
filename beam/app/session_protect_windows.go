//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// sessionSealed says the session file is encrypted on this platform.
const sessionSealed = true

// sealSession encrypts the session file's bytes to this Windows user account
// (DPAPI). The file holds a live panel session, and file modes are advisory on
// Windows: in plain JSON it was readable by anything running as the user, and
// it travelled with a roaming profile.
func sealSession(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(plain)), Data: &plain[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// openSession reverses sealSession. It fails for another user's file, or one
// copied from another machine, which is the point.
func openSession(sealed []byte) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(sealed)), Data: &sealed[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
