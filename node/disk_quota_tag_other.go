//go:build !linux

package main

import "errors"

// tagProjectTree: project quotas exist only on Linux; NewQuotaProvider never
// marks a provider available elsewhere.
func tagProjectTree(string, uint32) error {
	return errors.New("project quotas need Linux")
}

func setProjectTree(string, string, uint32) error {
	return errors.New("project quotas need Linux")
}
