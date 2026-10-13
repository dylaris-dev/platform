package services

import "fmt"

// NodeBusyKey mirrors the node's own key name (node/reconciler.go): the node
// holds it while it installs, updates, copies or restores a server. The node is
// the only writer; Core only reads it, and neither may rename it alone.
func NodeBusyKey(uuid string) string {
	return fmt.Sprintf("dylaris:server:%s:node_busy", uuid)
}
