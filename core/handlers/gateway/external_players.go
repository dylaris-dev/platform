package gateway

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"

	"dylaris-core/handlers/reqctx"
	"dylaris-core/services"
	"dylaris-core/store"
)

type externalPlayersTotal struct {
	Java    int `json:"java"`
	Bedrock int `json:"bedrock"`
}

type externalPlayersItem struct {
	Domain  string `json:"domain"`
	Java    int    `json:"java"`
	Bedrock int    `json:"bedrock"`
}

// playerPeakWindow is the highest concurrent players over a span of UTC days,
// each column its own maximum.
type playerPeakWindow struct {
	Java    int `json:"java"`
	Bedrock int `json:"bedrock"`
	Total   int `json:"total"`
}

// playerPeaks reads the owner's last 30 UTC days (today included) once: the
// rows newest first, today's window and the per-column maximum of all of them.
// No rows is zeros.
func playerPeaks(st store.Store, owner string, now time.Time) (rows []store.PlayerPeak, today, last30 playerPeakWindow, err error) {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	rows, err = st.ListPlayerPeaks(owner, day.AddDate(0, 0, -29))
	if err != nil {
		return nil, today, last30, err
	}
	for _, p := range rows {
		if p.Day.Equal(day) {
			today = playerPeakWindow{Java: p.Java, Bedrock: p.Bedrock, Total: p.Total}
		}
		last30.Java, last30.Bedrock, last30.Total = max(last30.Java, p.Java), max(last30.Bedrock, p.Bedrock), max(last30.Total, p.Total)
	}
	return rows, today, last30, nil
}

// GetPlayers GET /api/external/gateway/players - the live play sessions on the caller's route-only addresses, by edition, as the edges count them: total {java, bedrock}, items [{domain, java, bedrock}] for the addresses with at least one player (a suspended address is left out), asOf the time of the answer (a count can be about 12 seconds old), and peak {today, last30Days}, the highest concurrent players per UTC day, each a lower bound (at least that many). asOf is not the time of the samples: a 9 s sample plus the 3 s cache. An edge that does not report the count shows as none. In peak, java, bedrock and total are each their own maximum (total is the highest sum seen at once); sampled once a minute, so each is AT LEAST that many, zeros when nothing was recorded.
func (h *ExternalGatewayHandler) GetPlayers(w http.ResponseWriter, r *http.Request) {
	owner := reqctx.ByonCallerID(r)
	if owner == "" {
		writeExternalError(w, http.StatusUnauthorized, "Unauthorized", "unauthorized")
		return
	}
	rows, err := h.gw.d.Store.ListCoreLinkRoutesByOwner(owner)
	if err != nil {
		log.Printf("external gateway players for %s: %v", owner, err)
		writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
		return
	}
	items := []externalPlayersItem{}
	total := externalPlayersTotal{}
	if h.gw.d.Redis != nil {
		// Keyed by the caller's own rows: another account's address is never looked up.
		counts := services.GatewayPlayersByRoute(r.Context(), h.gw.d.Redis)
		for _, row := range rows {
			p, ok := counts[row.Domain]
			// A suspended address is left out, as in the peaks and the admin view.
			if !ok || p.Java+p.Bedrock == 0 || row.SuspendedAt != nil {
				continue
			}
			items = append(items, externalPlayersItem{Domain: row.Domain, Java: p.Java, Bedrock: p.Bedrock})
			total.Java += p.Java
			total.Bedrock += p.Bedrock
		}
		sort.Slice(items, func(a, b int) bool { return items[a].Domain < items[b].Domain })
	}
	now := time.Now().UTC()
	_, today, last30, err := playerPeaks(h.gw.d.Store, owner, now)
	if err != nil {
		log.Printf("external gateway player peaks for %s: %v", owner, err)
		writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "asOf": now.Format(time.RFC3339), "total": total, "items": items,
		"peak": map[string]playerPeakWindow{"today": today, "last30Days": last30}})
}
