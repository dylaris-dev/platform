package models

// Memory guard actions: what Core does when the node reports a server held at
// or above 98% of its container memory limit (non-reclaimable memory) for 30 seconds.
const (
	MemoryGuardStop    = "stop" // graceful stop that saves; stays stopped
	MemoryGuardRestart = "restart"
	// MemoryGuardOff is the default: warn only (the node still sends save-all).
	// A small plan's Paper server can sit at 98-99% non-reclaimable for good
	// without ever being OOM-killed, so acting on it must be a choice.
	MemoryGuardOff = "off"

	// CrashReasonOOMKilled is servers.last_crash_reason after an OOM kill.
	CrashReasonOOMKilled = "oom_killed"
)

// ValidMemoryGuardAction reports whether a may be stored.
func ValidMemoryGuardAction(a string) bool {
	return a == MemoryGuardStop || a == MemoryGuardRestart || a == MemoryGuardOff
}
