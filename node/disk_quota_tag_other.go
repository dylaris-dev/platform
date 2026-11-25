//go:build !linux

package main

import "errors"

// tagProjectTree: project quotas exist only on Linux; NewQuotaProvider never
// marks a provider available elsewhere.
func tagProjectTree(string, uint32) error {
	return errors.New("project quotas need Linux")
}

func projectTagsMoved(string, string, uint32) (bool, error) {
	return false, errors.New("project quotas need Linux")
}

func setBackupTree(string, uint32, bool) error {
	return errors.New("project quotas need Linux")
}

func backupTagsMoved(string, uint32, bool) (bool, error) {
	return false, errors.New("project quotas need Linux")
}
