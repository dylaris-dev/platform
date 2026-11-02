//go:build !linux

package validate

// PathIsOnContainerRootFS is not answerable off Linux. Core only ever RUNS as a
// Linux container; this file exists so the package still builds and tests on a
// developer machine. Reporting "not determinable" makes the caller skip the
// warning rather than emit a wrong one.
func PathIsOnContainerRootFS(string) (onRoot bool, determinable bool) {
	return false, false
}
