package packs

import (
	"fmt"
	"strings"
)

// Pack archives are extracted on Windows too, where case-only differences
// address the same file. Reject ambiguity instead of relying on ZIP order.
type packOutputPaths map[string]uint8

func (p packOutputPaths) add(name string) error {
	return p.addForSide(name, "both")
}

func contentSideMask(side string) uint8 {
	switch side {
	case "client":
		return 1
	case "server":
		return 2
	default:
		return 3
	}
}

func (p packOutputPaths) addForSide(name, side string) error {
	if !validContentTarget(name) {
		return fmt.Errorf("invalid pack output path %q", name)
	}
	key := strings.ToLower(name)
	mask := contentSideMask(side)
	if p[key]&mask != 0 {
		return fmt.Errorf("duplicate pack output path %q; remove the overlapping content or archive entry", name)
	}
	p[key] |= mask
	return nil
}
