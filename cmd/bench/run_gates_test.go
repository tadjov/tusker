package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/DjordjeVuckovic/tusker/internal/bench/pool"
	"github.com/DjordjeVuckovic/tusker/internal/bench/spec"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// judgedTrack is a track whose judgments grade every doc in its pool, served by
// engines that answer every query unless failing says otherwise.
func judgedTrack(t *testing.T, failing map[string]bool) lineageTrack {
	tr, _, _ := judgedTrackDocs(t, failing)
	return tr
}

func judgedTrackDocs(t *testing.T, failing map[string]bool) (tr lineageTrack, relevant, other uuid.UUID) {
	t.Helper()
	relevant, other = uuid.New(), uuid.New()
	b := &fakeIndex{count: 10, ranked: map[string][]uuid.UUID{"q1": {other, relevant}, "q2": {other}}}
	cluster := fakeCluster(t, map[string]*fakeIndex{
		"a": {count: 10, ranked: map[string][]uuid.UUID{"q1": {relevant, other}, "q2": {relevant}}},
		"b": b,
	})
	tr = newLineageTrack(t, cluster)
	tr.writeJudgments(t, tr.pool(t).Meta.RunID, map[string]map[uuid.UUID]int{
		"q1": {relevant: 3, other: 0},
		"q2": {relevant: 2, other: 0},
	})
	b.failing = failing
	return tr, relevant, other
}

// seedLatest points latest.json at an earlier complete report and returns the
// pointer's bytes, so a run that must leave latest.json alone can be checked
// against them.
func seedLatest(t *testing.T, tr lineageTrack) []byte {
	t.Helper()
	require.NoError(t, updateLatestPointer(tr.latestPath(), writeCleanReport(t, tr, "2026-01-01T00-00-00-seed-prior")))
	pointer, err := os.ReadFile(tr.latestPath())
	require.NoError(t, err)
	return pointer
}

func assertLatestUnchanged(t *testing.T, tr lineageTrack, before []byte) {
	t.Helper()
	after, err := os.ReadFile(tr.latestPath())
	require.NoError(t, err, "latest.json must survive the run")
	assert.Equal(t, string(before), string(after), "latest.json must not move")
}

func TestRun_CompleteRunMovesLatest(t *testing.T) {
	tr := judgedTrack(t, nil)

	_, err := execute(t, newRunCmd(), tr.dir)

	require.NoError(t, err)
	require.Len(t, tr.reports(t), 1)
	latest := tr.latestReport(t)
	require.NotNil(t, latest)
	assert.Equal(t, tr.reports(t)[0].Provenance.RunID, latest.Provenance.RunID)
}

func TestRun_MissingDefaultJudgmentsFailsBeforeRunning(t *testing.T) {
	tr := judgedTrack(t, nil)
	require.NoError(t, os.Remove(tr.judgments))
	before := seedLatest(t, tr)

	_, err := execute(t, newRunCmd(), tr.dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--latency-only")
	assert.Empty(t, tr.reports(t))
	assertLatestUnchanged(t, tr, before)
}

func TestRun_LatencyOnlyWritesAMarkedReportAndLeavesLatest(t *testing.T) {
	tr := judgedTrack(t, nil)
	require.NoError(t, os.Remove(tr.judgments))
	before := seedLatest(t, tr)

	_, err := execute(t, newRunCmd(), tr.dir, "--latency-only")

	require.NoError(t, err)
	reports := tr.reports(t)
	require.Len(t, reports, 1)
	assert.True(t, reports[0].Settings.LatencyOnly)
	assert.NotEmpty(t, reports[0].Settings.Incomplete)
	assertLatestUnchanged(t, tr, before)
}

func TestRun_LatencyOnlyRejectsExplicitJudgments(t *testing.T) {
	tr := judgedTrack(t, nil)

	_, err := execute(t, newRunCmd(), tr.dir, "--latency-only", "--judgments", tr.judgments)

	require.Error(t, err)
	assert.Empty(t, tr.reports(t))
}

func TestRun_FailedRunWritesTheReportButLeavesLatest(t *testing.T) {
	tests := []struct {
		name    string
		failing map[string]bool
		regrade func(relevant, other uuid.UUID) map[string]map[uuid.UUID]int
		wantErr string
	}{
		{name: "an engine errored on a query", failing: map[string]bool{"q2": true}, wantErr: "q2"},
		{
			name: "a pooled doc the judge failed to grade",
			regrade: func(relevant, other uuid.UUID) map[string]map[uuid.UUID]int {
				return map[string]map[uuid.UUID]int{"q1": {relevant: 3, other: -1}, "q2": {relevant: 2, other: 0}}
			},
			wantErr: "1 of 4 pooled docs",
		},
		{
			name: "a pooled query the judge never reached",
			regrade: func(relevant, other uuid.UUID) map[string]map[uuid.UUID]int {
				return map[string]map[uuid.UUID]int{"q1": {relevant: 3, other: 0}}
			},
			wantErr: "2 of 4 pooled docs",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, relevant, other := judgedTrackDocs(t, tt.failing)
			if tt.regrade != nil {
				tr.writeJudgments(t, tr.poolRunID(t), tt.regrade(relevant, other))
			}
			before := seedLatest(t, tr)

			_, err := execute(t, newRunCmd(), tr.dir)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Contains(t, err.Error(), "latest.json")
			reports := tr.reports(t)
			require.Len(t, reports, 1, "the report is kept for inspection")
			assert.NotEmpty(t, reports[0].Settings.Incomplete)
			assertLatestUnchanged(t, tr, before)
		})
	}
}

func TestRun_LatestMovesOnlyForTheSpecsOwnRun(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantMoved bool
	}{
		{name: "spec settings", wantMoved: true},
		{name: "every job named", args: []string{"--jobs", "a-only,both"}, wantMoved: true},
		{name: "iterations equal to the spec", args: []string{"--iterations", "2"}, wantMoved: true},
		{name: "a subset of jobs", args: []string{"--jobs", "a-only"}},
		{name: "more iterations", args: []string{"--iterations", "5"}},
		{name: "more warmup", args: []string{"--warmup", "3"}},
		{name: "other cut-offs", args: []string{"--k", "1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := judgedTrack(t, nil)
			before := seedLatest(t, tr)

			_, err := execute(t, newRunCmd(), append([]string{tr.dir}, tt.args...)...)

			require.NoError(t, err)
			reports := tr.reports(t)
			require.Len(t, reports, 1)
			if tt.wantMoved {
				assert.Equal(t, reports[0].Provenance.RunID, tr.latestReport(t).Provenance.RunID)
				assert.Empty(t, reports[0].Settings.Incomplete)
			} else {
				assertLatestUnchanged(t, tr, before)
				assert.NotEmpty(t, reports[0].Settings.Incomplete)
			}
		})
	}
}

func TestRun_ReportRecordsItsSettings(t *testing.T) {
	tr := judgedTrack(t, nil)

	_, err := execute(t, newRunCmd(), tr.dir, "--jobs", "a-only", "--iterations", "4")
	require.NoError(t, err)

	reports := tr.reports(t)
	require.Len(t, reports, 1)
	settings := reports[0].Settings
	assert.Equal(t, 1, settings.Warmup, "spec warmup")
	assert.Equal(t, 4, settings.Iterations)
	assert.Equal(t, spec.DefaultQueryTimeout, settings.QueryTimeout, "the spec sets no query_timeout")
	assert.Equal(t, []string{"a-only"}, settings.JobFilter)
	assert.Equal(t, map[string]any{"boost": float64(2)}, settings.EngineParams["a"], "a declares boost: 2 in the spec")
	assert.Equal(t, []int{1, 2}, reports[0].Config.KValues)
}

func TestRun_SpecDefaultJudgmentsPathIsRelativeToTheTrack(t *testing.T) {
	tr := judgedTrack(t, nil)
	specPath := filepath.Join(tr.dir, "spec.yaml")
	specYAML, err := os.ReadFile(specPath)
	require.NoError(t, err)
	relative := bytes.Replace(specYAML, []byte(strconv.Quote(tr.judgments)), []byte(`"trec/annotations.fixture.yaml"`), 1)
	require.NotEqual(t, specYAML, relative)
	require.NoError(t, os.WriteFile(specPath, relative, 0o644))

	_, err = execute(t, newRunCmd(), tr.dir)

	require.NoError(t, err, "the test runs from cmd/bench, so only the track folder holds trec/")
	assert.NotNil(t, tr.latestReport(t))
}

// A pool written with --allow-partial holds nothing from the cells it dropped,
// so a run measuring one of them would score its hits as non-relevant.
func TestRun_PartialPoolFailsTheCellsItDropped(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "the run measures the dropped cell", wantErr: true},
		{name: "the run leaves the dropped engine out", args: []string{"--jobs", "a-only"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			relevant, other, unpooled := uuid.New(), uuid.New(), uuid.New()
			b := &fakeIndex{
				count:   10,
				ranked:  map[string][]uuid.UUID{"q1": {other, relevant}, "q2": {unpooled, relevant}},
				failing: map[string]bool{"q2": true},
			}
			cluster := fakeCluster(t, map[string]*fakeIndex{
				"a": {count: 10, ranked: map[string][]uuid.UUID{"q1": {relevant, other}, "q2": {relevant}}},
				"b": b,
			})
			tr := newLineageTrack(t, cluster)
			_, err := execute(t, newPoolCmd(), tr.dir, "--allow-partial")
			require.NoError(t, err)
			pf, err := pool.ReadPoolFile(tr.poolPath())
			require.NoError(t, err)
			tr.writeJudgments(t, pf.Meta.RunID, map[string]map[uuid.UUID]int{
				"q1": {relevant: 3, other: 0},
				"q2": {relevant: 2},
			})
			b.failing = nil
			before := seedLatest(t, tr)

			_, err = execute(t, newRunCmd(), append([]string{tr.dir}, tt.args...)...)

			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "q2/b")
			assert.Contains(t, err.Error(), "re-pool without --allow-partial")
			assert.NotContains(t, err.Error(), "re-pool and re-judge", "the dropped cell is reported once, with its own cause")
			assertLatestUnchanged(t, tr, before)
		})
	}
}

func TestRun_SuiteQueryMissingFromThePoolFailsTheRun(t *testing.T) {
	tr := judgedTrack(t, nil)
	suitePath := filepath.Join(tr.dir, "suite.yaml")
	suiteYAML, err := os.ReadFile(suitePath)
	require.NoError(t, err)
	suiteYAML = append(suiteYAML, []byte(`  - id: q3
    engines:
      a: { query: '{"query": {"match": {"title": "q3"}}}' }
      b: { query: '{"query": {"match": {"title": "q3"}}}' }
`)...)
	require.NoError(t, os.WriteFile(suitePath, suiteYAML, 0o644))
	before := seedLatest(t, tr)

	_, err = execute(t, newRunCmd(), tr.dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "q3")
	assertLatestUnchanged(t, tr, before)
}

// An engine added to the spec after pooling contributed nothing to the pool,
// so its hits were never judged and would score as non-relevant.
func TestRun_EngineMissingFromThePoolFailsTheRun(t *testing.T) {
	tr := judgedTrack(t, nil)
	pf, err := pool.ReadPoolFile(tr.poolPath())
	require.NoError(t, err)
	pf.Meta.Engines = []string{"a"}
	require.NoError(t, pool.WritePoolFile(pf, tr.poolPath()))
	before := seedLatest(t, tr)

	_, err = execute(t, newRunCmd(), tr.dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "never pooled")
	assert.Contains(t, err.Error(), "b")
	assertLatestUnchanged(t, tr, before)
}

func TestRun_InputsOtherThanTheSpecsOwnLeaveLatest(t *testing.T) {
	tests := []struct {
		name      string
		args      func(tr lineageTrack) []string
		wantMoved bool
		wantWhy   string
	}{
		{
			name:      "the spec's default judgments named explicitly",
			args:      func(tr lineageTrack) []string { return []string{"--judgments", tr.judgments} },
			wantMoved: true,
		},
		{
			name: "another spec file",
			args: func(tr lineageTrack) []string {
				return []string{"--spec", copyFile(t, filepath.Join(tr.dir, "spec.yaml"), filepath.Join(tr.dir, "other.yaml"))}
			},
			wantWhy: "--spec",
		},
		{
			name: "another judgments file",
			args: func(tr lineageTrack) []string {
				return []string{"--judgments", copyFile(t, tr.judgments, filepath.Join(tr.dir, "trec", "annotations.other.yaml"))}
			},
			wantWhy: "--judgments",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := judgedTrack(t, nil)
			before := seedLatest(t, tr)

			_, err := execute(t, newRunCmd(), append([]string{tr.dir}, tt.args(tr)...)...)

			require.NoError(t, err)
			reports := tr.reports(t)
			require.Len(t, reports, 1)
			if tt.wantMoved {
				assert.Equal(t, reports[0].Provenance.RunID, tr.latestReport(t).Provenance.RunID)
				return
			}
			assertLatestUnchanged(t, tr, before)
			assert.Contains(t, strings.Join(reports[0].Settings.Incomplete, "; "), tt.wantWhy)
		})
	}
}

func copyFile(t *testing.T, from, to string) string {
	t.Helper()
	data, err := os.ReadFile(from)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(to, data, 0o644))
	return to
}

// The judge's own count misses a doc pooled after it last wrote, so the report
// records what the run itself found against the pool.
func TestRun_ReportRecordsTheUnjudgedDocsItFound(t *testing.T) {
	tr := judgedTrack(t, nil)
	pf, err := pool.ReadPoolFile(tr.poolPath())
	require.NoError(t, err)
	pf.Queries[0].Docs = append(pf.Queries[0].Docs, pool.PooledDoc{DocID: uuid.New(), Sources: []string{"a"}})
	require.NoError(t, pool.WritePoolFile(pf, tr.poolPath()))

	_, err = execute(t, newRunCmd(), tr.dir)
	require.Error(t, err)

	reports := tr.reports(t)
	require.Len(t, reports, 1)
	require.NotNil(t, reports[0].Provenance.UnjudgedCount)
	assert.Equal(t, 1, *reports[0].Provenance.UnjudgedCount)
}
