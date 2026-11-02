//go:build !linux

package main

// hideFromSameUID has nothing to do off Linux: the shipper only ever runs in a
// Linux container, this exists so the package builds for tests on a dev box.
func hideFromSameUID() error { return nil }
