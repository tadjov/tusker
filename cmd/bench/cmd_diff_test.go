package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DjordjeVuckovic/tusker/internal/bench/meta"
	"github.com/DjordjeVuckovic/tusker/internal/bench/report"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type diffRun struct {
	runID     string
	kValues   []int
	ndcg      map[int]float64
	threshold int
	judge     *meta.Judge
	judgments string
	spec      string
}

func diffReport(run diffRun) *report.Report {
	return &report.Report{
		SchemaVersion: 1,
		Provenance: meta.Meta{
			RunID:   run.runID,
			Judge:   run.judge,
			Sources: &meta.Sources{Judgments: run.judgments, Spec: run.spec},
		},
		Config: report.ReportConfig{KValues: run.kValues, RelevanceThreshold: run.threshold},
		Jobs: []report.JobReport{{
			JobName:    "fts",
			Aggregated: []report.AggregatedEntry{{EngineName: "pg", NDCG: run.ndcg, MAP: 0.5, JudgedCount: 1, QueryCount: 1}},
			PerQuery:   []report.Entry{{QueryID: "q1", EngineName: "pg", Judged: true, NDCG: run.ndcg}},
		}},
	}
}

func TestPrintDiff_ComparesOnlyKValuesBothRunsHave(t *testing.T) {
	before := diffReport(diffRun{runID: "a", kValues: []int{3, 5, 10}, ndcg: map[int]float64{3: 0.4, 5: 0.5, 10: 0.6}, threshold: 1})
	after := diffReport(diffRun{runID: "b", kValues: []int{3, 5, 20}, ndcg: map[int]float64{3: 0.4, 5: 0.55, 20: 0.7}, threshold: 1})

	var out bytes.Buffer
	printDiff(&out, before, after)
	got := out.String()

	assert.Contains(t, got, "NDCG@5")
	assert.Contains(t, got, "0.5000→0.5500")
	assert.NotContains(t, got, "NDCG@10")
	assert.NotContains(t, got, "NDCG@20")
	assert.NotContains(t, got, "0.6000→0.0000", "a K missing from one run must not read as a zero score")
}

func TestPrintDiff_SkipsNDCGWhenNoKValueIsShared(t *testing.T) {
	before := diffReport(diffRun{runID: "a", kValues: []int{10}, ndcg: map[int]float64{10: 0.6}, threshold: 1})
	after := diffReport(diffRun{runID: "b", kValues: []int{20}, ndcg: map[int]float64{20: 0.7}, threshold: 1})

	var out bytes.Buffer
	printDiff(&out, before, after)

	assert.NotContains(t, out.String(), "-100.0%")
	assert.NotContains(t, out.String(), "Per-Query NDCG")
}

func TestPrintDiff_WarnsWhenTheRunsWereScoredDifferently(t *testing.T) {
	lexical := &meta.Judge{Strategy: "lexical"}
	llm := &meta.Judge{Strategy: "llm-cli", Provider: "claude", Model: "claude-haiku-4-5"}
	base := diffRun{runID: "a", kValues: []int{10}, ndcg: map[int]float64{10: 0.6}, threshold: 1, judge: lexical, judgments: "trec/annotations.lexical.yaml"}

	tests := []struct {
		name    string
		after   func(diffRun) diffRun
		warning string
	}{
		{name: "judge", after: func(r diffRun) diffRun { r.judge = llm; return r }, warning: "judge"},
		{name: "judge prompt version", after: func(r diffRun) diffRun {
			r.judge = &meta.Judge{Strategy: "lexical", PromptVersion: "v2"}
			return r
		}, warning: "judge"},
		{name: "relevance threshold", after: func(r diffRun) diffRun { r.threshold = 2; return r }, warning: "relevance threshold"},
		{name: "judgments source", after: func(r diffRun) diffRun { r.judgments = "trec/annotations.llm-cli.yaml"; return r }, warning: "judgments"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			after := tc.after(base)
			after.runID = "b"

			var out bytes.Buffer
			printDiff(&out, diffReport(base), diffReport(after))
			got := out.String()

			warningAt := strings.Index(got, "WARNING")
			require.NotEqual(t, -1, warningAt, got)
			assert.Less(t, warningAt, strings.Index(got, "--- Job:"))
			assert.Contains(t, got[warningAt:strings.Index(got, "--- Job:")], tc.warning)
		})
	}
}

func TestPrintDiff_NoScoringWarningForIdenticallyScoredRuns(t *testing.T) {
	run := diffRun{runID: "a", kValues: []int{10}, ndcg: map[int]float64{10: 0.6}, threshold: 1, judge: &meta.Judge{Strategy: "lexical"}, judgments: "trec/annotations.lexical.yaml"}
	after := run
	after.runID = "b"

	var out bytes.Buffer
	printDiff(&out, diffReport(run), diffReport(after))

	assert.NotContains(t, out.String(), "WARNING")
}

func writeTrackReport(t *testing.T, dir, runID string, mutate func(*report.Report)) {
	t.Helper()
	r := diffReport(diffRun{runID: runID, kValues: []int{10}, ndcg: map[int]float64{10: 0.6}, threshold: 1})
	if mutate != nil {
		mutate(r)
	}
	require.NoError(t, report.WriteJSON(r, filepath.Join(dir, runID+".json")))
}

func TestExecuteDiff_DefaultsToLatestAndThePreviousFullyScoredRun(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	initCmd := newInitCmd()
	initCmd.SetOut(&bytes.Buffer{})
	initCmd.SetArgs([]string{"demo"})
	require.NoError(t, initCmd.Execute())
	reports := filepath.Join(cwd, "demo", "reports")

	latencyOnly := func(r *report.Report) {
		r.Jobs[0].Aggregated[0].JudgedCount = 0
		r.Jobs[0].PerQuery[0].Judged = false
	}
	errored := func(r *report.Report) {
		r.Jobs[0].Aggregated[0].ErrorCount = 1
		r.Jobs[0].PerQuery[0].Error = "timeout"
	}
	incomplete := func(r *report.Report) {
		r.Settings.Incomplete = []string{"--jobs selected a subset of the spec's jobs"}
	}
	writeTrackReport(t, reports, "2026-01-01T00-00-01-run-aaaaaa", nil)
	writeTrackReport(t, reports, "2026-01-01T00-00-02-run-bbbbbb", latencyOnly)
	writeTrackReport(t, reports, "2026-01-01T00-00-03-run-cccccc", errored)
	writeTrackReport(t, reports, "2026-01-01T00-00-03-run-ffffff", incomplete)
	writeTrackReport(t, reports, "2026-01-01T00-00-04-run-dddddd", nil)
	writeTrackReport(t, reports, "2026-01-01T00-00-05-run-eeeeee", errored)
	require.NoError(t, updateLatestPointer(filepath.Join(reports, "latest.json"),
		filepath.Join(reports, "2026-01-01T00-00-04-run-dddddd.json")))

	var out bytes.Buffer
	require.NoError(t, executeDiff(&out, diffFlags{}, []string{"demo"}))

	assert.Regexp(t, `A \(before\):\s+2026-01-01T00-00-01-run-aaaaaa`, out.String())
	assert.Regexp(t, `B \(after\):\s+\S*2026-01-01T00-00-04-run-dddddd`, out.String())
}

func TestPrintDiff_PerQueryDeltasCountAFailedQueryAsZero(t *testing.T) {
	before := diffReport(diffRun{runID: "a", kValues: []int{10}, ndcg: map[int]float64{10: 0.6}, threshold: 1})
	after := diffReport(diffRun{runID: "b", kValues: []int{10}, ndcg: map[int]float64{10: 0.6}, threshold: 1})
	after.Jobs[0].PerQuery[0] = report.Entry{QueryID: "q1", EngineName: "pg", Judged: true, Error: "timeout"}

	var out bytes.Buffer
	printDiff(&out, before, after)

	assert.Regexp(t, `(?m)^q1\s+pg\s+0\.6000\s+0\.0000\s+\S*-0\.6000`, out.String())
}

func TestPrintDiff_PerQueryDeltasIgnoreAFailedQueryThatRunNeverGraded(t *testing.T) {
	before := diffReport(diffRun{runID: "a", kValues: []int{10}, ndcg: map[int]float64{10: 0.6}, threshold: 1})
	after := diffReport(diffRun{runID: "b", kValues: []int{10}, ndcg: map[int]float64{10: 0.6}, threshold: 1})
	after.Jobs[0].PerQuery[0] = report.Entry{QueryID: "q1", EngineName: "pg", Error: "timeout"}

	var out bytes.Buffer
	printDiff(&out, before, after)

	assert.NotContains(t, out.String(), "Per-Query NDCG")
}

func scaffoldDiffTrack(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	t.Chdir(cwd)
	initCmd := newInitCmd()
	initCmd.SetOut(&bytes.Buffer{})
	initCmd.SetArgs([]string{"demo"})
	require.NoError(t, initCmd.Execute())
	return filepath.Join(cwd, "demo", "reports")
}

func TestExecuteDiff_BaselineSearchSkipsUnreadableReports(t *testing.T) {
	reports := scaffoldDiffTrack(t)
	writeTrackReport(t, reports, "2026-01-01T00-00-01-run-aaaaaa", nil)
	require.NoError(t, os.WriteFile(filepath.Join(reports, "2026-01-01T00-00-02-run-bbbbbb.json"), []byte("{not json"), 0o644))
	writeTrackReport(t, reports, "2026-01-01T00-00-03-run-cccccc", nil)
	require.NoError(t, updateLatestPointer(filepath.Join(reports, "latest.json"),
		filepath.Join(reports, "2026-01-01T00-00-03-run-cccccc.json")))

	var out bytes.Buffer
	require.NoError(t, executeDiff(&out, diffFlags{}, []string{"demo"}))
	assert.Regexp(t, `A \(before\):\s+2026-01-01T00-00-01-run-aaaaaa`, out.String())

	err := executeDiff(&bytes.Buffer{}, diffFlags{pathA: "2026-01-01T00-00-02-run-bbbbbb"}, []string{"demo"})
	assert.Error(t, err, "an explicitly named unreadable report is still an error")
}

func TestExecuteDiff_FallsBackToTheNewestEarlierReportWhenNoneIsFullyScored(t *testing.T) {
	reports := scaffoldDiffTrack(t)
	latencyOnly := func(r *report.Report) {
		r.Jobs[0].Aggregated[0].JudgedCount = 0
		r.Jobs[0].PerQuery[0].Judged = false
	}
	writeTrackReport(t, reports, "2026-01-01T00-00-01-run-aaaaaa", latencyOnly)
	writeTrackReport(t, reports, "2026-01-01T00-00-02-run-bbbbbb", latencyOnly)
	writeTrackReport(t, reports, "2026-01-01T00-00-03-run-cccccc", latencyOnly)
	require.NoError(t, updateLatestPointer(filepath.Join(reports, "latest.json"),
		filepath.Join(reports, "2026-01-01T00-00-03-run-cccccc.json")))

	var out bytes.Buffer
	require.NoError(t, executeDiff(&out, diffFlags{}, []string{"demo"}))

	assert.Regexp(t, `A \(before\):\s+2026-01-01T00-00-02-run-bbbbbb`, out.String())
}

func TestExecuteDiff_WithoutLatestPointerDiffsTheTwoNewestScoredReports(t *testing.T) {
	reports := scaffoldDiffTrack(t)
	writeTrackReport(t, reports, "2026-01-01T00-00-01-run-aaaaaa", nil)
	writeTrackReport(t, reports, "2026-01-01T00-00-02-run-bbbbbb", nil)
	require.NoFileExists(t, filepath.Join(reports, "latest.json"))

	var out bytes.Buffer
	require.NoError(t, executeDiff(&out, diffFlags{}, []string{"demo"}))

	assert.Regexp(t, `A \(before\):\s+2026-01-01T00-00-01-run-aaaaaa`, out.String())
	assert.Regexp(t, `B \(after\):\s+\S*2026-01-01T00-00-02-run-bbbbbb`, out.String())
}

func TestPrintDiff_NDCGHeadersNameTheGain(t *testing.T) {
	before := diffReport(diffRun{runID: "a", kValues: []int{10}, ndcg: map[int]float64{10: 0.6}, threshold: 1})
	after := diffReport(diffRun{runID: "b", kValues: []int{10}, ndcg: map[int]float64{10: 0.7}, threshold: 1})

	var out bytes.Buffer
	printDiff(&out, before, after)

	headers := regexp.MustCompile(`NDCG@\d+[^\t\n]*`).FindAllString(out.String(), -1)
	require.Len(t, headers, 2, "the aggregate column and the per-query title")
	for _, header := range headers {
		assert.Contains(t, header, "2^rel-1")
	}
}

func TestPrintDiff_SameJudgmentsFileInAMovedTrackIsNotAWarning(t *testing.T) {
	judge := &meta.Judge{Strategy: "lexical"}
	before := diffRun{
		runID: "a", kValues: []int{10}, ndcg: map[int]float64{10: 0.6}, threshold: 1, judge: judge,
		spec:      "/Users/me/projects/news-hunter/tracks/cc-news/fts/spec.yaml",
		judgments: "/Users/me/projects/news-hunter/tracks/cc-news/fts/trec/annotations.lexical.yaml",
	}
	moved := before
	moved.runID = "b"
	moved.spec = "/Users/me/projects/news-hunter-wt/fix-x/tracks/cc-news/fts/spec.yaml"
	moved.judgments = "/Users/me/projects/news-hunter-wt/fix-x/tracks/cc-news/fts/trec/annotations.lexical.yaml"
	swapped := moved
	swapped.judgments = "/Users/me/projects/news-hunter-wt/fix-x/tracks/cc-news/fts/trec/annotations.bm25.yaml"

	var same bytes.Buffer
	printDiff(&same, diffReport(before), diffReport(moved))
	assert.NotContains(t, same.String(), "WARNING")

	var different bytes.Buffer
	printDiff(&different, diffReport(before), diffReport(swapped))
	assert.Contains(t, different.String(), "judgments: trec/annotations.lexical.yaml → trec/annotations.bm25.yaml")
}

func TestPrintDiff_ChangeFromAZeroBaselineHasNoPercentage(t *testing.T) {
	before := diffReport(diffRun{runID: "a", kValues: []int{10}, ndcg: map[int]float64{10: 0}, threshold: 1})
	after := diffReport(diffRun{runID: "b", kValues: []int{10}, ndcg: map[int]float64{10: 0.4}, threshold: 1})
	after.Jobs[0].Aggregated[0].Latency = report.LatencyStats{Percentiles: map[int]time.Duration{50: 3 * time.Millisecond}}

	var out bytes.Buffer
	printDiff(&out, before, after)
	got := out.String()

	assert.Contains(t, got, "n/a")
	assert.NotContains(t, got, "+0.0%")
}
