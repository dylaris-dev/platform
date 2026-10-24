package stats

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/metrics"
	"dylaris-core/services"
)

// MetricsHandler serves the long-term record.
//
// Read-only throughout. The recorder is the only writer, and it is leader-gated
// inside Core; there is deliberately no endpoint that can insert a sample, so
// nothing reachable over HTTP can put a number into a record whose whole value
// is that it was measured.
type MetricsHandler struct {
	state *handlers.AppState
}

func NewMetricsHandler(state *handlers.AppState) *MetricsHandler {
	return &MetricsHandler{state: state}
}

// ready reports whether there is a record to read, and writes the reason if not.
//
// The two "no" cases are different and are told apart on purpose: a metrics
// database that failed to open is a problem to fix, while the feature simply
// being off is the DEFAULT and not a fault. A panel that could not tell them
// apart would send an operator looking for a broken database when nobody had
// switched recording on.
// It RETURNS the handle rather than letting each caller fetch its own. The
// target is swappable at runtime now, so a second read could land after a swap
// and hand a closed pool to a query this function had already approved.
func (h *MetricsHandler) ready(w http.ResponseWriter, r *http.Request) (*metrics.Handle, bool) {
	handle := h.state.Metrics.Handle()
	if handle == nil || handle.Read == nil {
		sendMetricsJSON(w, map[string]any{
			"success": true, "available": false, "reason": "unavailable",
			"message": "The metrics database is not reachable, so nothing is being recorded.",
		})
		return nil, false
	}
	if !h.state.FeatureFlags.Get(r.Context(), services.MetricsEnabledSetting, false) {
		sendMetricsJSON(w, map[string]any{
			"success": true, "available": false, "reason": "disabled",
			"message": "Long-term statistics are switched off. Turn them on in Settings, Features to start recording.",
		})
		return nil, false
	}
	return handle, true
}

// Catalog GET /api/admin/metrics/catalog
// The series this build records, plus how far back the record goes.
func (h *MetricsHandler) Catalog(w http.ResponseWriter, r *http.Request) {
	handle, ok := h.ready(w, r)
	if !ok {
		return
	}
	cov, err := metrics.ReadCoverage(r.Context(), handle.Read, handle.Resolution)
	if err != nil {
		handlers.SendJSONError(w, "Could not read the record: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sendMetricsJSON(w, map[string]any{
		"success":   true,
		"available": true,
		"series":    metrics.Catalog,
		"coverage":  cov,
	})
}

// Series GET /api/admin/metrics/series
//
//	?metric=platform.players&from=…&to=…&step=300&subject=…&region=…&split=1
func (h *MetricsHandler) Series(w http.ResponseWriter, r *http.Request) {
	handle, ok := h.ready(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	from, to, err := windowFrom(q)
	if err != nil {
		handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Several metrics in one request, because a chart page asks for a dozen and
	// a round trip each is the difference between one render and twelve.
	names := splitCSV(q.Get("metric"))
	if len(names) == 0 {
		handlers.SendJSONError(w, "Name at least one metric.", http.StatusBadRequest)
		return
	}
	if len(names) > 24 {
		handlers.SendJSONError(w, "Too many metrics in one request (24 max).", http.StatusBadRequest)
		return
	}

	step := time.Duration(0)
	if s, err := strconv.Atoi(q.Get("step")); err == nil && s > 0 {
		step = time.Duration(s) * time.Second
	}

	out := make([]metrics.SeriesResult, 0, len(names))
	for _, name := range names {
		res, err := metrics.Query(r.Context(), handle.Read, metrics.SeriesQuery{
			Metric:        name,
			From:          from,
			To:            to,
			Step:          step,
			Subject:       q.Get("subject"),
			Region:        q.Get("region"),
			SplitSubjects: q.Get("split") == "1",
		})
		if err != nil {
			handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
		out = append(out, res...)
	}
	sendMetricsJSON(w, map[string]any{
		"success": true, "available": true, "series": out,
		"from": from.UTC(), "to": to.UTC(),
	})
}

// Summary GET /api/admin/metrics/summary?from=&to=
// The headline numbers, reduced over the window.
func (h *MetricsHandler) Summary(w http.ResponseWriter, r *http.Request) {
	handle, ok := h.ready(w, r)
	if !ok {
		return
	}
	from, to, err := windowFrom(r.URL.Query())
	if err != nil {
		handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := metrics.Summary(r.Context(), handle.Read, from, to)
	if err != nil {
		handlers.SendJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cov, _ := metrics.ReadCoverage(r.Context(), handle.Read, handle.Resolution)
	sendMetricsJSON(w, map[string]any{
		"success": true, "available": true, "headlines": rows, "coverage": cov,
		"from": from.UTC(), "to": to.UTC(),
	})
}

// Export GET /api/admin/metrics/export?metric=…&from=&to=&step=&format=csv|json
//
// The point of the export is that the numbers can leave with the person reading
// them - into a spreadsheet, a due-diligence pack, a mail. It is the same query
// the charts run, so an export can never disagree with what was on screen.
func (h *MetricsHandler) Export(w http.ResponseWriter, r *http.Request) {
	handle, ok := h.ready(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	from, to, err := windowFrom(q)
	if err != nil {
		handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	names := splitCSV(q.Get("metric"))
	if len(names) == 0 {
		// No metric named means the whole catalog, which is what somebody
		// exporting "the statistics" means.
		for _, s := range metrics.Catalog {
			names = append(names, s.Metric)
		}
	}
	step := time.Duration(0)
	if s, err := strconv.Atoi(q.Get("step")); err == nil && s > 0 {
		step = time.Duration(s) * time.Second
	}

	var all []metrics.SeriesResult
	for _, name := range names {
		res, err := metrics.Query(r.Context(), handle.Read, metrics.SeriesQuery{
			Metric: name, From: from, To: to, Step: step,
			Subject: q.Get("subject"), Region: q.Get("region"),
			SplitSubjects: q.Get("split") == "1",
		})
		if err != nil {
			handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
		all = append(all, res...)
	}

	stamp := time.Now().UTC().Format("2006-01-02")
	if q.Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="dylaris-metrics-%s.json"`, stamp))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"exported": time.Now().UTC(), "from": from.UTC(), "to": to.UTC(), "series": all,
		})
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="dylaris-metrics-%s.csv"`, stamp))
	cw := csv.NewWriter(w)
	defer cw.Flush()
	// min/max/avg alongside the total, because which of them a series MEANS
	// depends on whether it is a counter or a gauge - and a spreadsheet that
	// only received one column could not recover the others.
	_ = cw.Write([]string{"metric", "subject", "time", "min", "max", "avg", "sum", "count"})
	for _, s := range all {
		for _, p := range s.Points {
			_ = cw.Write([]string{
				s.Metric, s.Subject, p.Time.Format(time.RFC3339),
				strconv.FormatFloat(p.Min, 'f', -1, 64),
				strconv.FormatFloat(p.Max, 'f', -1, 64),
				strconv.FormatFloat(p.Avg, 'f', -1, 64),
				strconv.FormatFloat(p.Sum, 'f', -1, 64),
				strconv.FormatInt(p.Count, 10),
			})
		}
	}
}

// windowFrom reads from/to, as RFC3339 or as a relative "24h"/"30d" in `range`.
func windowFrom(q map[string][]string) (time.Time, time.Time, error) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	to := time.Now()
	if v := get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("to is not an RFC3339 timestamp")
		}
		to = t
	}
	if v := get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("from is not an RFC3339 timestamp")
		}
		if !t.Before(to) {
			return time.Time{}, time.Time{}, fmt.Errorf("from is not before to")
		}
		return t, to, nil
	}
	// A relative range, which is what the panel's period picker sends. Days are
	// spelled out because "30d" is not something time.ParseDuration accepts.
	span := 24 * time.Hour
	if v := get("range"); v != "" {
		if strings.HasSuffix(v, "d") {
			n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
			if err != nil || n <= 0 {
				return time.Time{}, time.Time{}, fmt.Errorf("range is not a number of days")
			}
			span = time.Duration(n) * 24 * time.Hour
		} else {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return time.Time{}, time.Time{}, fmt.Errorf("range is not a duration")
			}
			span = d
		}
	}
	return to.Add(-span), to, nil
}

// sendMetricsJSON writes a 200 with a JSON body. Local to this file rather than
// a shared helper: every other handler here builds its own response, and one
// more general utility that only three call sites use is not worth the reach.
func sendMetricsJSON(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
