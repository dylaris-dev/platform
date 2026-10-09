package models

import (
	"strconv"
	"strings"
)

// RAM padding is the memory a container gets on top of the RAM a server was
// booked with: the JVM needs metaspace, thread stacks and native libraries
// outside its heap, and a container limited to the heap size alone gets
// OOM-killed under a big modpack. It is NOT a cap, so it does not follow the
// platform limit convention: nil means "inherit the next level", 0 means "no
// padding", n is n MB.
const (
	RAMPaddingSetting   = "placement.ram_padding_mb"
	DefaultRAMPaddingMB = 512
	MaxRAMPaddingMB     = 16384
)

// ValidRAMPaddingMB reports whether n may be stored at any level.
func ValidRAMPaddingMB(n int) bool { return n >= 0 && n <= MaxRAMPaddingMB }

// ParseGlobalRAMPaddingMB reads the stored global setting. Missing, empty or
// anything that is not a valid value reads as the default rather than as 0:
// a garbled setting must not strip every container of its headroom.
func ParseGlobalRAMPaddingMB(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || !ValidRAMPaddingMB(n) {
		return DefaultRAMPaddingMB
	}
	return n
}

// EffectiveRAMPaddingMB resolves server ?? node ?? global.
func EffectiveRAMPaddingMB(server, node *int, globalMB int) int {
	if server != nil {
		return *server
	}
	if node != nil {
		return *node
	}
	return globalMB
}
