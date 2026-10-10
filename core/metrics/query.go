package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Reading the record back.
//
// Every query re-aggregates stored buckets into the window the caller asked
// for, and the arithmetic is the same one the writer used: the smallest
// minimum, the largest maximum, the sums and counts added. That is what makes
// a year at minute resolution readable as a year: the chart asks for day
// buckets and the database does the folding.

// Point is one re-aggregated bucket.
type Point struct {
	Time  time.Time `json:"t"`
	Min   float64   `json:"min"`
	Max   float64   `json:"max"`
	Sum   float64   `json:"sum"`
	Count int64     `json:"count"`
	// Avg is Sum/Count, which is the average of the SAMPLES rather than of the
	// buckets. Those differ whenever buckets hold different numbers of samples,
	// and the sample average is the true one.
	Avg float64 `json:"avg"`
}

// SeriesQuery asks for one metric over a window.
type SeriesQuery struct {
	Metric  string
	From    time.Time
	To      time.Time
	Step    time.Duration
	Subject string // "" means every subject folded together
	Region  string // "" means every region
	// SplitSubjects returns one series per subject instead of one folded series.
	SplitSubjects bool
}

// SeriesResult is one line on a chart.
type SeriesResult struct {
	Metric  string  `json:"metric"`
	Subject string  `json:"subject,omitempty"`
	Points  []Point `json:"points"`
}

const (
	// maxPoints bounds one answer. A year of minute buckets is half a million
	// points, which no chart draws and no browser should be sent; the step is
	// widened until the answer fits.
	maxPoints = 2000
	// minStep is the finest bucket that can exist in either backend.
	minStep = time.Minute
)

// normalize clamps the window and picks a step that produces a drawable answer.
//
// The step is WIDENED rather than the window truncated, because a reader asking
// for a year wants the year - shown coarsely - and not the last three days
// shown finely. Silently returning a different window is the kind of answer
// that gets read as data.
func (q *SeriesQuery) normalize() error {
	if q.Metric == "" {
		return fmt.Errorf("no metric named")
	}
	if q.To.IsZero() {
		q.To = time.Now()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-24 * time.Hour)
	}
	if !q.From.Before(q.To) {
		return fmt.Errorf("the window ends before it starts")
	}
	if q.Step < minStep {
		q.Step = minStep
	}
	span := q.To.Sub(q.From)
	if want := span / maxPoints; want > q.Step {
		// Round up to a whole minute so buckets stay aligned to the grid the
		// writer used.
		q.Step = (want/minStep + 1) * minStep
	}
	return nil
}

// Query reads one metric.
func Query(ctx context.Context, db *sql.DB, q SeriesQuery) ([]SeriesResult, error) {
	if db == nil {
		return nil, fmt.Errorf("no metrics database")
	}
	if err := q.normalize(); err != nil {
		return nil, err
	}

	// The bucket expression floors the sample time onto a grid of q.Step
	// seconds, which is how stored minute (or hour) buckets fold into whatever
	// the caller asked for. Aligned to the epoch rather than to `from`, so two
	// charts over different windows put the same event in the same bucket.
	sel := `to_timestamp(floor(extract(epoch from time) / $1) * $1) AS bucket`
	group := "bucket"
	if q.SplitSubjects {
		sel += ", subject"
		group = "bucket, subject"
	} else {
		sel += ", '' AS subject"
	}

	where := "metric = $2 AND time >= $3 AND time < $4"
	args := []any{int64(q.Step.Seconds()), q.Metric, q.From.UTC(), q.To.UTC()}
	if q.Subject != "" {
		args = append(args, q.Subject)
		where += fmt.Sprintf(" AND subject = $%d", len(args))
	}
	if q.Region != "" {
		args = append(args, q.Region)
		where += fmt.Sprintf(" AND region = $%d", len(args))
	}

	sql := fmt.Sprintf(`
		SELECT %s, min(min_v), max(max_v), sum(sum_v), sum(count_v)
		FROM metric_samples
		WHERE %s
		GROUP BY %s
		ORDER BY bucket`, sel, where, group)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", q.Metric, err)
	}
	defer rows.Close()

	bySubject := map[string]*SeriesResult{}
	var order []string
	for rows.Next() {
		var (
			at      time.Time
			subject string
			p       Point
		)
		if err := rows.Scan(&at, &subject, &p.Min, &p.Max, &p.Sum, &p.Count); err != nil {
			return nil, err
		}
		p.Time = at.UTC()
		if p.Count > 0 {
			p.Avg = p.Sum / float64(p.Count)
		}
		s, ok := bySubject[subject]
		if !ok {
			s = &SeriesResult{Metric: q.Metric, Subject: subject}
			bySubject[subject] = s
			order = append(order, subject)
		}
		s.Points = append(s.Points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]SeriesResult, 0, len(order))
	for _, k := range order {
		out = append(out, *bySubject[k])
	}
	return out, nil
}

// Coverage is what the record actually holds.
type Coverage struct {
	// Since is the first sample ever recorded - the date the platform started
	// keeping this record, and the number that makes every other one credible.
	Since  *time.Time `json:"since"`
	Latest *time.Time `json:"latest"`
	// Resolution is the bucket size being written now.
	Resolution string `json:"resolution"`
}

// ReadCoverage answers "how far back does this go".
func ReadCoverage(ctx context.Context, db *sql.DB, res time.Duration) (Coverage, error) {
	out := Coverage{Resolution: res.String()}
	if db == nil {
		return out, fmt.Errorf("no metrics database")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// min() and max() over the time index. Cheap regardless of table size, and
	// the reason no DISTINCT-over-metric query exists anywhere here.
	var since, latest sql.NullTime
	err := db.QueryRowContext(ctx, `SELECT min(time), max(time) FROM metric_samples`).Scan(&since, &latest)
	if err != nil {
		return out, err
	}
	if since.Valid {
		t := since.Time.UTC()
		out.Since = &t
	}
	if latest.Valid {
		t := latest.Time.UTC()
		out.Latest = &t
	}
	return out, nil
}

// Headline is one number worth putting in front of somebody.
type Headline struct {
	Metric string  `json:"metric"`
	Label  string  `json:"label"`
	Unit   Unit    `json:"unit"`
	Value  float64 `json:"value"`
	// How the value was reduced: "peak", "total" or "avg". Shown, because a
	// peak and an average of the same series are different claims.
	How string `json:"how"`
	// Help says what the number does NOT mean, where the label alone misleads.
	Help string `json:"help,omitempty"`
}

// headlineSpec is one row of the summary, and how to reduce it.
type headlineSpec struct {
	metric string
	label  string
	how    string
	help   string
}

// headlineSpecs is the summary, in the order it is read.
//
// Chosen to answer the question the record exists for - what did this platform
// withstand - rather than to show everything. A peak is the load it actually
// carried; a total is the work it actually did.
var headlineSpecs = []headlineSpec{
	{"platform.players_online", "Peak players online", "peak", ""},
	{"platform.concurrent_users", "Peak concurrent users", "peak", ""},
	{"platform.servers_online", "Peak servers running", "peak", ""},
	{"platform.player_tx_bps", "Peak player throughput", "peak", ""},
	{"platform.bps_per_player", "Peak traffic per player (in + out)", "peak", ""},
	{"splice.sessions_opened", "Gateway connections", "total", gatewayConnectionsHelp},
	{"splice.handover_ok", "Players carried through an edge restart", "total", ""},
	{"splice.players_dropped", "Players dropped in a handover", "total", ""},
	// "Edge restarts", not "survived": this series counts restarts. Whether
	// anybody came through one is the line above, which measures it.
	{"edge.restarts", "Edge restarts", "total", ""},
	{"beam.transfers_started", "Beam transfers", "total", ""},
	{"node.up", "Node availability", "avg", ""},
	{"edge.up", "Edge availability", "avg", ""},
}

// Summary reduces the headline metrics over a window.
//
// One query per metric rather than one big one: they reduce differently, the
// list is short, and each is an indexed range over a single metric. A union
// would be harder to read for no measurable gain.
func Summary(ctx context.Context, db *sql.DB, from, to time.Time) ([]Headline, error) {
	if db == nil {
		return nil, fmt.Errorf("no metrics database")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out := make([]Headline, 0, len(headlineSpecs))
	for _, spec := range headlineSpecs {
		var v sql.NullFloat64
		var err error
		switch spec.how {
		case "peak":
			err = db.QueryRowContext(ctx,
				`SELECT max(max_v) FROM metric_samples WHERE metric = $1 AND time >= $2 AND time < $3`,
				spec.metric, from.UTC(), to.UTC()).Scan(&v)
		case "total":
			err = db.QueryRowContext(ctx,
				`SELECT sum(sum_v) FROM metric_samples WHERE metric = $1 AND time >= $2 AND time < $3`,
				spec.metric, from.UTC(), to.UTC()).Scan(&v)
		default:
			// The average of the SAMPLES, not of the buckets. For an
			// availability series recorded as 1/0 this is the uptime fraction,
			// and averaging bucket averages would weight a quiet minute the
			// same as a busy one.
			err = db.QueryRowContext(ctx,
				`SELECT sum(sum_v) / NULLIF(sum(count_v), 0) FROM metric_samples
				 WHERE metric = $1 AND time >= $2 AND time < $3`,
				spec.metric, from.UTC(), to.UTC()).Scan(&v)
		}
		if err != nil {
			return nil, fmt.Errorf("summary %s: %w", spec.metric, err)
		}
		if !v.Valid {
			// Never recorded in this window. Omitted rather than shown as zero:
			// "no players were dropped" and "nothing was measuring" are opposite
			// claims and a 0 would state the flattering one.
			continue
		}
		s, _ := Known(spec.metric)
		val := v.Float64
		if s.Unit == UnitPercent && spec.how == "avg" {
			val *= 100 // a 1/0 availability series reads as a percentage
		}
		out = append(out, Headline{
			Metric: spec.metric, Label: spec.label, Unit: s.Unit, Value: val, How: spec.how, Help: spec.help,
		})
	}
	return out, nil
}
