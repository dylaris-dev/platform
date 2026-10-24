package handlers

import (
	"context"
	"net/http"
	"strings"

	"dylaris-core/services"
)

// Cross-Minecraft-version availability for a modpack build and for a modded
// server. Both endpoints answer the same question and return the same shape,
// which is what lets one panel component render either: "if this moved to
// Minecraft X, which content would still have a version, and which would be
// lost".
//
// The per-version work lives in services/modcompat.go. These handlers only
// build the item list, which is the one thing the two surfaces genuinely differ
// on: a pack build carries the side its author declared, a server has to ask
// Modrinth what sides its mods serve.

// availabilityCache hands the compat service the same metadata cache the
// Modrinth proxy uses, so an operator who moved that cache off the control
// plane moved this one with it. A nil cache is read as "nothing cached" rather
// than as a failure.
func (s *AppState) availabilityCache() services.AvailabilityCache {
	if s == nil || s.Cache == nil {
		return nil
	}
	return s.Cache
}

// compatQuery reads the shared mode/mc query parameters.
func compatQuery(r *http.Request) (mode, specific string) {
	mode = strings.TrimSpace(r.URL.Query().Get("mode"))
	if mode == "" {
		mode = services.CompatModeAllNewer
	}
	return mode, strings.TrimSpace(r.URL.Query().Get("mc"))
}

// resolveTargets turns the request's mode into a concrete target list using
// Modrinth's own release ordering.
func resolveTargets(ctx context.Context, current, mode, specific string) ([]string, error) {
	tags, err := services.FetchGameVersions(ctx)
	if err != nil {
		return nil, err
	}
	return services.SelectCompatTargets(services.ReleaseVersions(tags), current, mode, specific), nil
}
