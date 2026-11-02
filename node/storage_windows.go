package main

import "golang.org/x/sys/windows"

// getDiskSpace returns (total, free) bytes for the filesystem at path.
func getDiskSpace(path string) (uint64, uint64) {
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0
	}
	err = windows.GetDiskFreeSpaceEx(pathPtr, &freeBytesAvailable, &totalBytes, &totalFreeBytes)
	if err != nil {
		return 0, 0
	}
	return totalBytes, freeBytesAvailable
}

// getInodes has no meaning on Windows; 0 is "unknown".
func getInodes(string) (uint64, uint64) { return 0, 0 }
