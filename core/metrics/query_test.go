package metrics

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeWidensTheStepRatherThanTheWindow(t *testing.T) {
	// A year at minute resolution is half a million points. Returning the last
	// few days at the requested step would be a different answer wearing the
	// same label - and a chart titled "last 12 months" showing three days is
	// how a number gets misread.
	to := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	q := SeriesQuery{Metric: "platform.players", From: to.AddDate(-1, 0, 0), To: to, Step: time.Minute}
	if err := q.normalize(); err != nil {
		t.Fatal(err)
	}
	if !q.From.Equal(to.AddDate(-1, 0, 0)) || !q.To.Equal(to) {
		t.Fatalf("the window moved: %v..%v", q.From, q.To)
	}
	if points := q.To.Sub(q.From) / q.Step; points > maxPoints {
		t.Fatalf("step %v still yields %d points, cap is %d", q.Step, points, maxPoints)
	}
	if q.Step%time.Minute != 0 {
		t.Fatalf("step %v is not a whole number of minutes, so buckets fall off the recorded grid", q.Step)
	}
}

func TestNormalizeKeepsASmallWindowFine(t *testing.T) {
	to := time.Now()
	q := SeriesQuery{Metric: "m", From: to.Add(-time.Hour), To: to, Step: time.Minute}
	if err := q.normalize(); err != nil {
		t.Fatal(err)
	}
	if q.Step != time.Minute {
		t.Fatalf("step widened to %v for a one-hour window", q.Step)
	}
}

func TestNormalizeRejectsAnInvertedWindow(t *testing.T) {
	now := time.Now()
	q := SeriesQuery{Metric: "m", From: now, To: now.Add(-time.Hour)}
	if err := q.normalize(); err == nil {
		t.Fatal("a window that ends before it starts was accepted")
	}
}

func TestNormalizeNeedsAMetric(t *testing.T) {
	q := SeriesQuery{}
	if err := q.normalize(); err == nil {
		t.Fatal("a query with no metric was accepted")
	}
}

func TestAnUnknownMetricIsReadableAndTreatedAsAGauge(t *testing.T) {
	// The gateway contract lets a component publish a name this build has never
	// heard of. Refusing to chart it would make new telemetry invisible until
	// Core was rebuilt, which defeats the point of a contract that never changes.
	s, known := Known("splice.something_new")
	if known {
		t.Fatal("that name is in the catalog; pick one that is not")
	}
	if s.Kind != KindGauge {
		// Averaging a counter understates it; SUMMING a gauge produces a number
		// with no meaning at all. Gauge is the safer wrong answer.
		t.Fatalf("unknown metric defaulted to %q", s.Kind)
	}
	if s.Label == "" {
		t.Error("an unknown metric has no label, so a chart would have a blank legend")
	}
}

// Every series the collector records has to appear in the catalog.
//
// The catalog is what the picker, the units and the counter/gauge distinction
// are read from, so a series recorded but not catalogued is invisible: it fills
// the table for months and never appears on a screen. Nothing else would fail.
func TestEverySeriesRecordedIsInTheCatalog(t *testing.T) {
	recorded := recordedMetricNames(t)
	if len(recorded) < 20 {
		t.Fatalf("only found %d recorded metric names; the extraction is broken, not the catalog", len(recorded))
	}
	for name, where := range recorded {
		if _, ok := catalogByMetric[name]; !ok {
			t.Errorf("%s records %q, which is not in the catalog - it would be stored for years and shown nowhere",
				where, name)
		}
	}
}

// And the reverse, for the series this repo produces: a catalogued name that
// nothing records is a chart that is permanently empty. Only the platform-side
// prefixes are checked, because the gateway names come from the other
// repository and are not visible here.
func TestNoPlatformSeriesIsCataloguedWithoutAProducer(t *testing.T) {
	recorded := recordedMetricNames(t)
	for _, s := range Catalog {
		switch {
		case strings.HasPrefix(s.Metric, "platform."),
			strings.HasPrefix(s.Metric, "node."),
			strings.HasPrefix(s.Metric, "core."),
			strings.HasPrefix(s.Metric, "postgres."),
			strings.HasPrefix(s.Metric, "redis."):
		default:
			continue // edge./splice./warp./link./beam. are produced in the gateway repo
		}
		if _, ok := recorded[s.Metric]; !ok {
			t.Errorf("the catalog offers %q and nothing in this repository records it; the chart would always be empty", s.Metric)
		}
	}
}

// recordedMetricNames returns every metric name the collector records, mapped
// to the file it came from.
//
// Two shapes, because the collector has two. Most names are the first argument
// of a c.obs(...) call. The Postgres and Redis series are the string KEYS of a
// map literal that the collector loops over, which is the right code and would
// be worse written out one call at a time - so the keys are collected too.
//
// Scoped to services/metrics_*.go rather than the whole package: a dotted
// lowercase string is a plausible shape for something that is not a metric, and
// a test that demands a catalog entry for an unrelated constant is a test people
// learn to edit rather than to read.
func recordedMetricNames(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join("..", "services")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v (if the collector moved, this test moves with it - do not delete it)", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "metrics_") ||
			!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "obs" || len(node.Args) == 0 {
					return true
				}
				// A computed name is the gateway pass-through: not statically
				// checkable, and catalogued by hand.
				if v, ok := stringLit(node.Args[0]); ok {
					out[v] = name
				}
			case *ast.CompositeLit:
				// Either side of the pair, because the collector writes both
				// shapes: the Postgres map has metric names as KEYS (name to
				// current value), the Redis map has them as VALUES (INFO field
				// to metric name). The dot is what tells a metric name from an
				// INFO field, which never has one.
				for _, el := range node.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					for _, side := range []ast.Expr{kv.Key, kv.Value} {
						if v, ok := stringLit(side); ok && strings.Contains(v, ".") {
							out[v] = name
						}
					}
				}
			}
			return true
		})
	}
	if files == 0 {
		t.Fatal("no services/metrics_*.go files found; the extraction is looking in the wrong place")
	}
	return out
}

// stringLit returns the value of a string literal expression.
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING || len(lit.Value) < 2 {
		return "", false
	}
	return lit.Value[1 : len(lit.Value)-1], true
}

func TestTheCatalogHasNoDuplicatesAndNamesEveryField(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Catalog {
		if seen[s.Metric] {
			// A duplicate would be silently dropped by the index, and the entry
			// that survived would be whichever came last.
			t.Errorf("%s appears twice in the catalog", s.Metric)
		}
		seen[s.Metric] = true
		if s.Label == "" || s.Group == "" || s.Kind == "" || s.Unit == "" {
			t.Errorf("%s is missing a label, group, kind or unit: %+v", s.Metric, s)
		}
	}
}

func TestHeadlinesOnlyNameCataloguedSeries(t *testing.T) {
	// A headline whose metric is not catalogued has no unit, so it would be
	// rendered as a bare number - and the difference between 4e9 bytes and
	// 4e9 players is the whole meaning of the card.
	for _, h := range headlineSpecs {
		if _, ok := catalogByMetric[h.metric]; !ok {
			t.Errorf("headline %q names %q, which is not in the catalog", h.label, h.metric)
		}
	}
}

func TestTheHeadlinesNameNoSpliceSeries(t *testing.T) {
	// sessions_opened counts every TCP connection and players_dropped every
	// session the edge closed under a live client - refusals, scanners, kicks.
	// Neither is a number about players, so neither is a headline.
	for _, h := range headlineSpecs {
		if strings.HasPrefix(h.metric, "splice.") || strings.HasPrefix(h.minus, "splice.") {
			t.Errorf("headline %q is built on %q, a splice series", h.label, h.metric)
		}
		if h.metric == "edge.logins" {
			t.Errorf("edge.logins has a headline card; the owner decided against one for now")
		}
	}
}

func TestBeamHeadlineCountsOnlyRoutedConnections(t *testing.T) {
	// The relay counts every accepted connection as started and every unrouted
	// one as failed; in production 410 of 413 were refusals.
	var found bool
	for _, h := range headlineSpecs {
		if h.metric != "beam.transfers_started" {
			continue
		}
		found = true
		if h.how != "total" || h.minus != "beam.transfers_failed" {
			t.Fatalf("beam headline = %+v, want a total of transfers_started minus transfers_failed", h)
		}
	}
	if !found {
		t.Fatal("no beam headline")
	}
	for _, h := range headlineSpecs {
		if h.minus != "" {
			if _, ok := catalogByMetric[h.minus]; !ok {
				t.Errorf("headline %q subtracts %q, which is not in the catalog", h.label, h.minus)
			}
			if h.how != "total" {
				t.Errorf("headline %q subtracts from a %q; only a total can be netted", h.label, h.how)
			}
		}
	}
}

func TestNetTotal(t *testing.T) {
	for _, tc := range []struct{ total, minus, want float64 }{
		{413, 410, 3},
		{5, 0, 5},
		{0, 0, 0},
		// A connection still open at the window's edge counts as started in
		// one window and failed in the next.
		{2, 3, 0},
	} {
		if got := netTotal(tc.total, tc.minus); got != tc.want {
			t.Errorf("netTotal(%v, %v) = %v, want %v", tc.total, tc.minus, got, tc.want)
		}
	}
}
