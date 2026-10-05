//go:build !unix

package main

import (
	"io/fs"
	"os"
)

// ownedBy always reports false where file ownership is not a uid.
//
// False is the safe answer: it makes the caller walk and chown, and os.Lchown
// then fails harmlessly on a platform that has no such concept. The alternative,
// returning true, would silently SKIP the ownership fix - and this file exists
// only so a Windows build compiles, never so it decides anything.
func ownedBy(os.FileInfo, int) bool { return false }

// multiplyLinked has no meaning where ownership is not a uid.
func multiplyLinked(fs.DirEntry) bool { return false }

func multiplyLinkedInfo(os.FileInfo) bool { return false }
