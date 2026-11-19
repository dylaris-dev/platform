//go:build !linux

package main

import "os"

// openLevelByFd has no short name to reach a descriptor by here; the caller
// falls back to Root.OpenRoot. Nodes run on Linux.
func openLevelByFd(*os.Root, string) (*os.Root, func(), bool) { return nil, nil, false }
