package database

import (
	"context"
	"dylaris-core/metrics"
	"fmt"
	"testing"
	"time"
)

// Reading the record back is where the arithmetic can quietly go wrong, and no
// fake can prove it: the folding of stored buckets into the window a chart asks
// for is done by Postgres, in SQL, with a bucket expression and four aggregates.
func TestIntegrationQueryFoldsStoredBucketsIntoTheRequestedStep(t *testing.T) {
	db, _ := integrationDB(t)
	ctx := context.Background()
	if err := metrics.EnsureSchema(ctx, db, false); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	metric := fmt.Sprintf("test.query.%d", time.Now().UnixNano())
	t.Cleanup(func() { db.Exec(`DELETE FROM metric_samples WHERE metric = $1`, metric) })

	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	st := metrics.NewSQLStore(db)

	// Five one-minute buckets, all inside one five-minute window.
	rows := []metrics.Row{
		{Time: base, Key: metrics.Key{Metric: metric}, Bucket: metrics.Bucket{Min: 2, Max: 8, Sum: 10, Count: 2}},
		{Time: base.Add(time.Minute), Key: metrics.Key{Metric: metric}, Bucket: metrics.Bucket{Min: 1, Max: 4, Sum: 5, Count: 2}},
		{Time: base.Add(2 * time.Minute), Key: metrics.Key{Metric: metric}, Bucket: metrics.Bucket{Min: 6, Max: 9, Sum: 15, Count: 2}},
		{Time: base.Add(3 * time.Minute), Key: metrics.Key{Metric: metric}, Bucket: metrics.Bucket{Min: 3, Max: 3, Sum: 3, Count: 1}},
		{Time: base.Add(4 * time.Minute), Key: metrics.Key{Metric: metric}, Bucket: metrics.Bucket{Min: 7, Max: 7, Sum: 7, Count: 1}},
	}
	if err := st.Upsert(ctx, rows); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := metrics.Query(ctx, db, metrics.SeriesQuery{
		Metric: metric,
		From:   base.Add(-time.Minute),
		To:     base.Add(10 * time.Minute),
		Step:   5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d series, want 1", len(got))
	}
	if len(got[0].Points) != 1 {
		t.Fatalf("five one-minute buckets folded into %d five-minute points, want 1", len(got[0].Points))
	}
	p := got[0].Points[0]

	// Exactly what merging all five in memory produces. The two paths agreeing
	// is the property; a difference means a chart shows something the recorder
	// never observed.
	var want metrics.Bucket
	for _, r := range rows {
		want = want.Merge(r.Bucket)
	}
	if p.Min != want.Min || p.Max != want.Max || p.Sum != want.Sum || p.Count != want.Count {
		t.Fatalf("folded to %+v, in-memory merge says %+v", p, want)
	}
	// The average has to be over SAMPLES, not over buckets. Here the buckets
	// hold 2,2,2,1,1 samples, so a mean of the five bucket averages would be a
	// different - and wrong - number.
	if wantAvg := want.Sum / float64(want.Count); p.Avg != wantAvg {
		t.Fatalf("avg = %v, want %v (sum/count, not the mean of the bucket means)", p.Avg, wantAvg)
	}
}

func TestIntegrationQueryFoldsSubjectsTogetherUnlessAskedToSplit(t *testing.T) {
	db, _ := integrationDB(t)
	ctx := context.Background()
	if err := metrics.EnsureSchema(ctx, db, false); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	metric := fmt.Sprintf("test.split.q.%d", time.Now().UnixNano())
	t.Cleanup(func() { db.Exec(`DELETE FROM metric_samples WHERE metric = $1`, metric) })

	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	one := metrics.Bucket{Min: 5, Max: 5, Sum: 5, Count: 1}
	if err := metrics.NewSQLStore(db).Upsert(ctx, []metrics.Row{
		{Time: at, Key: metrics.Key{Metric: metric, Subject: "edge-a"}, Bucket: one},
		{Time: at, Key: metrics.Key{Metric: metric, Subject: "edge-b"}, Bucket: one},
	}); err != nil {
		t.Fatal(err)
	}

	// Folded: one line for the fleet. The sum is what a counter means across
	// components, so it has to be 10 and not 5.
	folded, err := metrics.Query(ctx, db, metrics.SeriesQuery{
		Metric: metric, From: at.Add(-time.Minute), To: at.Add(time.Minute), Step: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(folded) != 1 || len(folded[0].Points) != 1 || folded[0].Points[0].Sum != 10 {
		t.Fatalf("folded query gave %+v, want one series summing to 10", folded)
	}

	// Split: one line per component, so a fleet chart can name the outlier.
	split, err := metrics.Query(ctx, db, metrics.SeriesQuery{
		Metric: metric, From: at.Add(-time.Minute), To: at.Add(time.Minute),
		Step: time.Minute, SplitSubjects: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(split) != 2 {
		t.Fatalf("split query gave %d series, want 2", len(split))
	}
	for _, s := range split {
		if s.Subject == "" {
			t.Error("a split series lost its subject, so the lines cannot be told apart")
		}
	}
}

func TestIntegrationSubjectFilterSelectsOneComponent(t *testing.T) {
	db, _ := integrationDB(t)
	ctx := context.Background()
	if err := metrics.EnsureSchema(ctx, db, false); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	metric := fmt.Sprintf("test.subj.%d", time.Now().UnixNano())
	t.Cleanup(func() { db.Exec(`DELETE FROM metric_samples WHERE metric = $1`, metric) })

	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := metrics.NewSQLStore(db).Upsert(ctx, []metrics.Row{
		{Time: at, Key: metrics.Key{Metric: metric, Subject: "a", Region: "eu"}, Bucket: metrics.Bucket{Min: 1, Max: 1, Sum: 1, Count: 1}},
		{Time: at, Key: metrics.Key{Metric: metric, Subject: "b", Region: "sg"}, Bucket: metrics.Bucket{Min: 9, Max: 9, Sum: 9, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := metrics.Query(ctx, db, metrics.SeriesQuery{
		Metric: metric, Subject: "b", From: at.Add(-time.Minute), To: at.Add(time.Minute), Step: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Points) != 1 || got[0].Points[0].Sum != 9 {
		t.Fatalf("subject filter gave %+v, want only b", got)
	}

	byRegion, err := metrics.Query(ctx, db, metrics.SeriesQuery{
		Metric: metric, Region: "eu", From: at.Add(-time.Minute), To: at.Add(time.Minute), Step: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byRegion) != 1 || byRegion[0].Points[0].Sum != 1 {
		t.Fatalf("region filter gave %+v, want only eu", byRegion)
	}
}

func TestIntegrationSummaryReducesEachHeadlineItsOwnWay(t *testing.T) {
	// A peak and a total are different claims about the same series, and the
	// summary makes both. Reducing one the other way is not a rounding error:
	// it is the difference between "we carried 400 players at once" and "we
	// served 400 players over a month".
	db, _ := integrationDB(t)
	ctx := context.Background()
	if err := metrics.EnsureSchema(ctx, db, false); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM metric_samples WHERE metric IN ('platform.players_online','splice.handover_ok','node.up')`)
	})

	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	st := metrics.NewSQLStore(db)
	if err := st.Upsert(ctx, []metrics.Row{
		// Peak 40, never 60 in total.
		{Time: at, Key: metrics.Key{Metric: "platform.players_online"}, Bucket: metrics.Bucket{Min: 10, Max: 40, Sum: 50, Count: 2}},
		{Time: at.Add(time.Minute), Key: metrics.Key{Metric: "platform.players_online"}, Bucket: metrics.Bucket{Min: 20, Max: 30, Sum: 50, Count: 2}},
		// Total 7 handovers, never a peak of 5.
		{Time: at, Key: metrics.Key{Metric: "splice.handover_ok", Subject: "h1"}, Bucket: metrics.Bucket{Min: 0, Max: 5, Sum: 5, Count: 2}},
		{Time: at.Add(time.Minute), Key: metrics.Key{Metric: "splice.handover_ok", Subject: "h1"}, Bucket: metrics.Bucket{Min: 2, Max: 2, Sum: 2, Count: 1}},
		// Three samples: up, up, down. Availability is two thirds.
		{Time: at, Key: metrics.Key{Metric: "node.up", Subject: "n1"}, Bucket: metrics.Bucket{Min: 0, Max: 1, Sum: 2, Count: 3}},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := metrics.Summary(ctx, db, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	got := map[string]float64{}
	how := map[string]string{}
	for _, h := range rows {
		got[h.Metric] = h.Value
		how[h.Metric] = h.How
	}

	if got["platform.players_online"] != 40 || how["platform.players_online"] != "peak" {
		t.Errorf("players = %v (%s), want 40 as a peak", got["platform.players_online"], how["platform.players_online"])
	}
	if got["splice.handover_ok"] != 7 || how["splice.handover_ok"] != "total" {
		t.Errorf("handovers = %v (%s), want 7 as a total", got["splice.handover_ok"], how["splice.handover_ok"])
	}
	// Recorded as 1/0 and reported as a percentage, because that is how anyone
	// reads an availability figure.
	if v := got["node.up"]; v < 66.6 || v > 66.7 {
		t.Errorf("node availability = %v, want ~66.67", v)
	}
}

func TestIntegrationSummaryOmitsAMetricNeverRecorded(t *testing.T) {
	// Zero and "nothing was measuring" are opposite claims, and zero is the
	// flattering one: a summary that showed "0 players dropped" for a month
	// nobody recorded would be a false statement made by a chart.
	db, _ := integrationDB(t)
	ctx := context.Background()
	if err := metrics.EnsureSchema(ctx, db, false); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	// A window far in the past, where nothing was ever written.
	far := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	rows, err := metrics.Summary(ctx, db, far, far.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a window with no samples produced %d headlines: %+v", len(rows), rows)
	}
}
