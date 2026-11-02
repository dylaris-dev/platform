package modcompat

import (
	"strconv"
	"strings"
)

// NeoForgeMajor maps a NeoForge build to the Minecraft version it targets, and
// says no to builds the picker must not offer.
//
// "21.1.106" targets MC 1.21.x, so the first number maps to "1.{N}". Since MC
// 26.x the scheme follows Minecraft itself: "26.3.0.89" targets 26.3, which the
// old "1."+first rule called "1.26". Pre-releases (-beta, -alpha..., +snapshot)
// are left out like PaperMC's, and so are the 0.x April-fools builds
// ("0.25w14craftmine.3-beta"), which made a Minecraft "1.0" appear.
func NeoForgeMajor(v string) (string, bool) {
	if strings.ContainsAny(v, "-+") {
		return "", false
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return "", false
	}
	first, err := strconv.Atoi(parts[0])
	if err != nil || first == 0 {
		return "", false
	}
	if first >= 26 {
		return parts[0] + "." + parts[1], true
	}
	return "1." + parts[0], true
}
