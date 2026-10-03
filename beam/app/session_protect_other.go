//go:build !windows

package main

// sessionSealed says the session file is encrypted on this platform. Outside
// Windows the 0600 mode is what protects it.
const sessionSealed = false

func sealSession(plain []byte) ([]byte, error) { return plain, nil }

func openSession(sealed []byte) ([]byte, error) { return sealed, nil }
