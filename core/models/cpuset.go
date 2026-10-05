package models

// EffectiveCpuset is the cpuset Core sends to the node for a server: the server's
// own pinned cpuset (auto/manual) when set, otherwise the node's static default.
// This keeps 'shared'/unpinned servers on the node default (prior behavior) while
// preserving a server's pinning across every recreate path (setup, switch,
// resource change, routing-mode switch).
func EffectiveCpuset(mode, serverCpuset, nodeCpuset string) string {
	if (mode == "auto" || mode == "manual") && serverCpuset != "" {
		return serverCpuset
	}
	return nodeCpuset
}
