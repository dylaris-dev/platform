//go:build !linux

package main

import "io/fs"

type fileIdentity struct{}

// identityOf has nothing to go on off Linux; every name is archived in full.
func identityOf(fs.FileInfo) (fileIdentity, bool) { return fileIdentity{}, false }
