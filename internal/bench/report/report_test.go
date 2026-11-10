package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/bench/metrics"
	"github.com/tadjov/tusker/internal/bench/runner"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

func makeJudgedScores(ndcg10 float64) metrics.ScoreSet {
	return metrics.ScoreSet{
		Judged:    true,
		NDCG:      map[int]float64{10: ndcg10},
		Precision: map[int]float64{10: 0.3},
		Recall:    map[int]float64{10: 0.4},
		F1:        map[int]float64{10: 0.34},
		AP:        ndcg10 * 0.9,
		RR:        1.0,
		Bpref:     ndcg10 * 0.8,
	}
}

func makeBenchmarkResult(engines []string, queryIDs []string, scores map[string]map[string]metrics.ScoreSet) *runner.BenchmarkResult {
	jr := &runner.JobResult{
		JobName:     "test_job",
		EngineNames: engines,
		QueryOrder:  queryIDs,
		Results:     make(map[string]map[string]runner.QueryResult),
	}
	for _, qid := range queryIDs {
		jr.Results[qid] = make(map[string]runner.QueryResult)
		for _, eng := range engines {
			sc := metrics.ScoreSet{}
			if scores != nil {
				if qScores, ok := scores[qid]; ok {
					sc = qScores[eng]
				}
			}
			jr.Results[qid][eng] = runner.QueryResult{
				QueryID:    qid,
				EngineName: eng,
				Scores:     sc,
				Latency: runner.ComputeLatencyStats([]time.Duration{
					10 * time.Millisecond,
					20 * time.Millisecond,
					50 * time.Millisecond,
				}),
			}
		}
	}
	return &runner.BenchmarkResult{
		Jobs: []*runner.JobResult{jr},
		Config: runner.Config{
			KValues:            []int{10},
			RelevanceThreshold: 1,
		},
	}
}

// ─── Generate ─────────────────────────────────────────────────────────────────

func TestGenerate_SchemaVersion(t *testing.T) {
	br := makeBenchmarkResult([]string{"eng-a"}, []string{"q1"}, nil)
	r := Generate(br, nil)
	if r.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", r.SchemaVersion)
	}
}

func TestGenerate_ProvenanceRunID(t *testing.T) {
	br := makeBenchmarkResult([]string{"eng-a"}, []string{"q1"}, nil)
	r := Generate(br, nil)
	if r.Provenance.RunID == "" {
		t.Error("Provenance.RunID should not be empty")
	}
}

func TestGenerate_JobsCount(t *testing.T) {
	br := makeBenchmarkResult([]string{"eng-a", "eng-b"}, []string{"q1", "q2"}, nil)
	r := Generate(br, nil)
	if got := len(r.Jobs); got != 1 {
		t.Errorf("len(Jobs) = %d, want 1", got)
	}
}

func TestGenerate_AggregatedMetrics(t *testing.T) {
	engines := []string{"pg", "es"}
	queries := []string{"q1", "q2", "q3"}
	scores := map[string]map[string]metrics.ScoreSet{
		"q1": {"pg": makeJudgedScores(0.8), "es": makeJudgedScores(0.6)},
		"q2": {"pg": makeJudgedScores(0.7), "es": makeJudgedScores(0.65)},
		"q3": {"pg": makeJudgedScores(0.9), "es": makeJudgedScores(0.7)},
	}
	br := makeBenchmarkResult(engines, queries, scores)
	r := Generate(br, nil)

	aggByEng := make(map[string]AggregatedEntry)
	for _, a := range r.Jobs[0].Aggregated {
		aggByEng[a.EngineName] = a
	}

	pgNDCG := aggByEng["pg"].NDCG[10]
	want := (0.8 + 0.7 + 0.9) / 3
	if diff := pgNDCG - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("pg NDCG@10 = %.6f, want %.6f", pgNDCG, want)
	}

	esNDCG := aggByEng["es"].NDCG[10]
	wantES := (0.6 + 0.65 + 0.7) / 3
	if diff := esNDCG - wantES; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("es NDCG@10 = %.6f, want %.6f", esNDCG, wantES)
	}

	if aggByEng["pg"].JudgedCount != 3 {
		t.Errorf("pg JudgedCount = %d, want 3", aggByEng["pg"].JudgedCount)
	}
}

func TestGenerate_LatencyPopulated(t *testing.T) {
	br := makeBenchmarkResult([]string{"eng"}, []string{"q1"}, nil)
	r := Generate(br, nil)
	agg := r.Jobs[0].Aggregated[0]
	if agg.Latency.SampleCount == 0 {
		t.Error("Latency.SampleCount should be > 0")
	}
	if agg.Latency.Mean == 0 {
		t.Error("Latency.Mean should be > 0")
	}
}

// ─── PerQuery entries ─────────────────────────────────────────────────────────

func TestGenerate_PerQueryEntries(t *testing.T) {
	engines := []string{"eng-a", "eng-b"}
	queries := []string{"q1", "q2"}
	br := makeBenchmarkResult(engines, queries, nil)
	r := Generate(br, nil)

	wantEntries := len(engines) * len(queries)
	if got := len(r.Jobs[0].PerQuery); got != wantEntries {
		t.Errorf("len(PerQuery) = %d, want %d", got, wantEntries)
	}
}

// ─── JSON round-trip ──────────────────────────────────────────────────────────

func TestWriteReadJSON_RoundTrip(t *testing.T) {
	br := makeBenchmarkResult([]string{"pg", "es"}, []string{"q1", "q2"}, map[string]map[string]metrics.ScoreSet{
		"q1": {"pg": makeJudgedScores(0.7), "es": makeJudgedScores(0.5)},
		"q2": {"pg": makeJudgedScores(0.6), "es": makeJudgedScores(0.55)},
	})
	orig := Generate(br, nil)
	orig.Provenance.SpecID = "round_trip_test"

	tmp := t.TempDir()
	path := filepath.Join(tmp, "report.json")

	if err := WriteJSON(orig, path); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	got, err := ReadJSON(path)
	if err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}

	if got.SchemaVersion != orig.SchemaVersion {
		t.Errorf("SchemaVersion: got %d want %d", got.SchemaVersion, orig.SchemaVersion)
	}
	if got.Provenance.RunID != orig.Provenance.RunID {
		t.Errorf("RunID: got %q want %q", got.Provenance.RunID, orig.Provenance.RunID)
	}
	if got.Provenance.SpecID != "round_trip_test" {
		t.Errorf("SpecID: got %q want round_trip_test", got.Provenance.SpecID)
	}
	if len(got.Jobs) != len(orig.Jobs) {
		t.Fatalf("len(Jobs): got %d want %d", len(got.Jobs), len(orig.Jobs))
	}
}

func TestWriteJSON_IsValidJSON(t *testing.T) {
	br := makeBenchmarkResult([]string{"eng"}, []string{"q1"}, nil)
	r := Generate(br, nil)

	tmp := t.TempDir()
	path := filepath.Join(tmp, "r.json")
	if err := WriteJSON(r, path); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	data, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if _, ok := m["schema_version"]; !ok {
		t.Error("JSON missing 'schema_version' key")
	}
	if _, ok := m["provenance"]; !ok {
		t.Error("JSON missing 'provenance' key")
	}
}

func TestReadLatestReport(t *testing.T) {
	br := makeBenchmarkResult([]string{"eng"}, []string{"q1"}, nil)
	r := Generate(br, nil)
	r.Provenance.RunID = "2026-05-28T00-00-00-run-test01"

	tmp := t.TempDir()
	reportPath := filepath.Join(tmp, r.Provenance.RunID+".json")
	latestPath := filepath.Join(tmp, "latest.json")

	if err := WriteJSON(r, reportPath); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	latestData := []byte(`{"latest": "` + r.Provenance.RunID + `.json"}`)
	if err := os.WriteFile(latestPath, latestData, 0644); err != nil {
		t.Fatalf("write latest.json: %v", err)
	}

	got, err := ReadLatestReport(latestPath)
	if err != nil {
		t.Fatalf("ReadLatestReport: %v", err)
	}
	if got.Provenance.RunID != r.Provenance.RunID {
		t.Errorf("RunID = %q, want %q", got.Provenance.RunID, r.Provenance.RunID)
	}
}

// ─── WriteTable smoke test ─────────────────────────────────────────────────────

func TestWriteTable_NoPanic(t *testing.T) {
	br := makeBenchmarkResult([]string{"pg", "es"}, []string{"q1", "q2"}, map[string]map[string]metrics.ScoreSet{
		"q1": {"pg": makeJudgedScores(0.8), "es": makeJudgedScores(0.6)},
		"q2": {"pg": makeJudgedScores(0.7), "es": makeJudgedScores(0.65)},
	})
	r := Generate(br, nil)

	var buf bytes.Buffer
	WriteTable(r, &buf)

	if buf.Len() == 0 {
		t.Error("WriteTable produced empty output")
	}
}

func TestWriteTable_ContainsEngineNames(t *testing.T) {
	br := makeBenchmarkResult([]string{"postgres", "elastic"}, []string{"q1"}, map[string]map[string]metrics.ScoreSet{
		"q1": {"postgres": makeJudgedScores(0.9), "elastic": makeJudgedScores(0.7)},
	})
	r := Generate(br, nil)

	var buf bytes.Buffer
	WriteTable(r, &buf)

	out := buf.String()
	for _, eng := range []string{"postgres", "elastic"} {
		if !strings.Contains(out, eng) {
			t.Errorf("table output missing engine name %q", eng)
		}
	}
}

// ─── WriteMarkdown smoke test ─────────────────────────────────────────────────

func TestWriteMarkdown_NoPanic(t *testing.T) {
	br := makeBenchmarkResult([]string{"pg", "es"}, []string{"q1", "q2"}, map[string]map[string]metrics.ScoreSet{
		"q1": {"pg": makeJudgedScores(0.8), "es": makeJudgedScores(0.6)},
		"q2": {"pg": makeJudgedScores(0.7), "es": makeJudgedScores(0.65)},
	})
	r := Generate(br, nil)

	var buf bytes.Buffer
	WriteMarkdown(r, &buf)

	if buf.Len() == 0 {
		t.Error("WriteMarkdown produced empty output")
	}
}

func TestWriteMarkdown_HasGFMTables(t *testing.T) {
	br := makeBenchmarkResult([]string{"eng-a"}, []string{"q1"}, map[string]map[string]metrics.ScoreSet{
		"q1": {"eng-a": makeJudgedScores(0.75)},
	})
	r := Generate(br, nil)
	r.Provenance.SpecID = "md_test"

	var buf bytes.Buffer
	WriteMarkdown(r, &buf)

	out := buf.String()
	if !strings.Contains(out, "| Engine |") {
		t.Error("WriteMarkdown output missing GFM table header '| Engine |'")
	}
	if !strings.Contains(out, "# md_test") {
		t.Error("WriteMarkdown output missing H1 title")
	}
}

// ─── RenderHTML smoke test ────────────────────────────────────────────────────

func TestWriteHTML_NoPanic(t *testing.T) {
	br := makeBenchmarkResult([]string{"pg", "es"}, []string{"q1", "q2"}, map[string]map[string]metrics.ScoreSet{
		"q1": {"pg": makeJudgedScores(0.8), "es": makeJudgedScores(0.6)},
		"q2": {"pg": makeJudgedScores(0.7), "es": makeJudgedScores(0.65)},
	})
	r := Generate(br, nil)

	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "report.html")

	if err := WriteHTML(r, outPath); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read HTML output: %v", err)
	}
	if !strings.Contains(string(data), "<html") {
		t.Error("HTML output missing <html tag")
	}
}

// ─── Significance ────────────────────────────────────────────────────────────

func TestGenerate_SignificancePopulated_EnoughData(t *testing.T) {
	// Need ≥4 non-tied pairs for Wilcoxon to produce a result.
	engines := []string{"eng-a", "eng-b"}
	nQueries := 10
	queries := make([]string, nQueries)
	scores := make(map[string]map[string]metrics.ScoreSet, nQueries)
	for i := 0; i < nQueries; i++ {
		qid := "q" + string(rune('0'+i))
		queries[i] = qid
		scoreA := 0.5 + float64(i)*0.03
		scoreB := 0.4 + float64(i)*0.02
		scores[qid] = map[string]metrics.ScoreSet{
			"eng-a": makeJudgedScores(scoreA),
			"eng-b": makeJudgedScores(scoreB),
		}
	}
	br := makeBenchmarkResult(engines, queries, scores)
	r := Generate(br, nil)
	// With 10 distinct pairs we should have at least one significance entry.
	if len(r.Jobs[0].Significance) == 0 {
		t.Error("expected at least one significance entry with 10 judged query pairs")
	}
}

// ─── LatencyStats helpers ─────────────────────────────────────────────────────

func TestLatencyStats_Percentile_Helpers(t *testing.T) {
	ls := LatencyStats{
		Percentiles: map[int]time.Duration{
			50: 20 * time.Millisecond,
			75: 30 * time.Millisecond,
			90: 40 * time.Millisecond,
			95: 45 * time.Millisecond,
			99: 50 * time.Millisecond,
		},
	}
	if got := ls.P50(); got != 20*time.Millisecond {
		t.Errorf("P50 = %v, want 20ms", got)
	}
	if got := ls.P99(); got != 50*time.Millisecond {
		t.Errorf("P99 = %v, want 50ms", got)
	}
}

// ─── fmtDuration ─────────────────────────────────────────────────────────────

func TestFmtDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "-"},
		{500 * time.Microsecond, "500.0µs"},
		{1500 * time.Microsecond, "1.50ms"},
		{2 * time.Second, "2.00s"},
	}
	for _, c := range cases {
		got := fmtDuration(c.d)
		if got != c.want {
			t.Errorf("fmtDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// ─── connectionEndpoint ───────────────────────────────────────────────────────

func TestConnectionEndpoint_DropsCredentials(t *testing.T) {
	tests := []struct {
		name string
		conn string
		want string
	}{
		{"short postgres url with password", "postgresql://u:secret@db:5432/news", "postgresql://db:5432/news"},
		{"long postgres url with password", "postgresql://news_user:news_password@localhost:54320/news_db", "postgresql://localhost:54320/news_db"},
		{"password in query string", "postgres://localhost:54320/news_db?password=secret&sslmode=disable", "postgres://localhost:54320/news_db"},
		{"elasticsearch url with basic auth", "http://elastic:changeme@localhost:9200", "http://localhost:9200"},
		{"plain elasticsearch url", "http://localhost:9200", "http://localhost:9200"},
		{"key value dsn", "host=localhost port=54320 user=news_user password=secret dbname=news_db", "host=localhost port=54320 dbname=news_db"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connectionEndpoint(tt.conn); got != tt.want {
				t.Errorf("connectionEndpoint(%q) = %q, want %q", tt.conn, got, tt.want)
			}
		})
	}
}

// ─── sampleStddev ─────────────────────────────────────────────────────────────

func TestSampleStddev(t *testing.T) {
	if got := sampleStddev([]float64{1.0}); got != 0 {
		t.Errorf("sampleStddev single value = %v, want 0", got)
	}

	// Population variance of {1,2,3,4,5} = 2; sample variance = 2.5; stddev ≈ 1.5811
	got := sampleStddev([]float64{1, 2, 3, 4, 5})
	want := 1.5811388300841898
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("sampleStddev = %v, want %v", got, want)
	}
}

// ─── hasAnyJudgments ─────────────────────────────────────────────────────────

func TestHasAnyJudgments(t *testing.T) {
	noJudge := &JobReport{PerQuery: []Entry{{Judged: false}, {Judged: false}}}
	if hasAnyJudgments(noJudge) {
		t.Error("hasAnyJudgments should be false when none are judged")
	}

	withJudge := &JobReport{PerQuery: []Entry{{Judged: false}, {Judged: true}}}
	if !hasAnyJudgments(withJudge) {
		t.Error("hasAnyJudgments should be true when at least one is judged")
	}
}

// ─── UUID-keyed metrics compatibility check ───────────────────────────────────

func TestGenerate_WithUUIDJudgedDocs(t *testing.T) {
	// Verify that ranked doc IDs with real UUIDs produce non-zero NDCG when
	// the judgments map is populated with the same UUIDs.
	docIDs := []uuid.UUID{
		uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		uuid.MustParse("22222222-2222-2222-2222-222222222222"),
	}
	judgments := map[uuid.UUID]int{
		docIDs[0]: 3, // highly relevant
		docIDs[1]: 1, // marginally relevant
	}
	kVals := []int{10}
	scores := metrics.ComputeAll(docIDs, judgments, kVals, 1)
	if !scores.Judged {
		t.Error("ScoreSet.Judged should be true with populated judgments")
	}
	if scores.NDCG[10] == 0 {
		t.Error("NDCG@10 should be > 0 with relevant docs")
	}
}

// ─── Per-category aggregation ────────────────────────────────────────────────

func withCategories(br *runner.BenchmarkResult, byQuery map[string]string) *runner.BenchmarkResult {
	jr := br.Jobs[0]
	for qid, results := range jr.Results {
		for eng, qr := range results {
			qr.Category = byQuery[qid]
			results[eng] = qr
		}
	}
	return br
}

func TestGenerate_ByCategorySplitsQueries(t *testing.T) {
	engines := []string{"pg"}
	queries := []string{"kw1", "kw2", "ph1"}
	scores := map[string]map[string]metrics.ScoreSet{
		"kw1": {"pg": makeJudgedScores(0.9)},
		"kw2": {"pg": makeJudgedScores(0.7)},
		"ph1": {"pg": makeJudgedScores(0.2)},
	}
	br := withCategories(makeBenchmarkResult(engines, queries, scores), map[string]string{
		"kw1": "keyword", "kw2": "keyword", "ph1": "phrase",
	})
	r := Generate(br, nil)

	byCat := make(map[string]CategoryReport)
	for _, cr := range r.Jobs[0].ByCategory {
		byCat[cr.Category] = cr
	}
	if len(byCat) != 2 {
		t.Fatalf("got %d categories, want 2", len(byCat))
	}
	if got := byCat["keyword"].QueryCount; got != 2 {
		t.Errorf("keyword QueryCount = %d, want 2", got)
	}

	kw := byCat["keyword"].Aggregated[0].NDCG[10]
	if diff := kw - 0.8; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("keyword NDCG@10 = %.6f, want 0.8", kw)
	}
	ph := byCat["phrase"].Aggregated[0].NDCG[10]
	if diff := ph - 0.2; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("phrase NDCG@10 = %.6f, want 0.2", ph)
	}

	overall := r.Jobs[0].Aggregated[0].NDCG[10]
	want := (0.9 + 0.7 + 0.2) / 3
	if diff := overall - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("overall NDCG@10 = %.6f, want %.6f", overall, want)
	}
}

func TestGenerate_ByCategoryEmptyWhenUntagged(t *testing.T) {
	br := makeBenchmarkResult([]string{"pg"}, []string{"q1", "q2"}, nil)
	r := Generate(br, nil)
	if got := len(r.Jobs[0].ByCategory); got != 0 {
		t.Errorf("len(ByCategory) = %d, want 0 for an untagged suite", got)
	}
}

func TestGenerate_ByCategoryEmptyForSingleCategory(t *testing.T) {
	br := withCategories(makeBenchmarkResult([]string{"pg"}, []string{"q1", "q2"}, nil),
		map[string]string{"q1": "keyword", "q2": "keyword"})
	r := Generate(br, nil)
	if got := len(r.Jobs[0].ByCategory); got != 0 {
		t.Errorf("len(ByCategory) = %d, want 0 when every query shares one category", got)
	}
}

func TestWriteJSON_CreatesMissingReportsDirectory(t *testing.T) {
	br := makeBenchmarkResult([]string{"pg"}, []string{"q1"}, nil)
	path := filepath.Join(t.TempDir(), "reports", "2026-05-28T00-00-00-run-test01.json")

	if err := WriteJSON(Generate(br, nil), path); err != nil {
		t.Fatalf("WriteJSON into a track with no reports/ directory: %v", err)
	}
	if _, err := ReadJSON(path); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
}

func TestReadJSON_RejectsReportWithoutSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte(`{"provenance":{"spec_id":"x"}}`), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if _, err := ReadJSON(path); err == nil {
		t.Fatal("ReadJSON accepted a report with no schema_version")
	}
}
