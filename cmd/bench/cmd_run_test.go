package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/judgment"
	"github.com/tadjov/tusker/internal/bench/meta"
	"github.com/tadjov/tusker/internal/bench/runner"
	"github.com/tadjov/tusker/internal/bench/spec"
	"github.com/tadjov/tusker/internal/bench/trackctx"
)

func TestLoadRunJudgments_NoneConfiguredNeedsLatencyOnly(t *testing.T) {
	tr := newLineageTrack(t, "http://unused.invalid")
	resolved, err := trackctx.Resolve(trackctx.Inputs{TrackArg: tr.dir})
	require.NoError(t, err)

	_, err = loadRunJudgments(runFlags{}, &spec.BenchSpec{}, resolved)
	require.Error(t, err, "a spec without default judgments and no --judgments has nothing to score against")
	assert.Contains(t, err.Error(), "--latency-only")

	judgments, err := loadRunJudgments(runFlags{latencyOnly: true}, &spec.BenchSpec{}, resolved)
	require.NoError(t, err)
	assert.Nil(t, judgments.grades)
}

func TestLoadJudgmentsMap_MissingFileExplicitErrors(t *testing.T) {
	_, _, err := loadJudgmentsMap(filepath.Join(t.TempDir(), "nope.yaml"), true)
	require.Error(t, err, "explicit --judgments path must error when file missing")
	assert.Contains(t, err.Error(), "--judgments file not found")
	assert.Contains(t, err.Error(), "keyword", "error should list valid strategies")
}

func TestLoadJudgmentsMap_MissingFileImplicitErrors(t *testing.T) {
	_, _, err := loadJudgmentsMap(filepath.Join(t.TempDir(), "nope.yaml"), false)
	require.Error(t, err, "a missing spec-default judgments file must not yield an unscored report")
	assert.Contains(t, err.Error(), "spec.defaults.judgments")
}

func TestLoadJudgmentsMap_FlattensAndFiltersUnjudged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ann.yaml")
	docOK := uuid.New()
	docSkip := uuid.New()

	jf := &judgment.File{
		Meta: meta.New("judge"),
		Queries: []judgment.Entry{
			{
				QueryID: "q1",
				Docs: []judgment.GradedDoc{
					{DocID: docOK, Grade: 2},
					{DocID: docSkip, Grade: -1},
				},
			},
		},
	}
	require.NoError(t, judgment.WriteFile(jf, path))

	m, jm, err := loadJudgmentsMap(path, true)
	require.NoError(t, err)
	require.NotNil(t, jm, "the grader identity must come back so the report can record it")
	assert.Equal(t, jf.Meta.RunID, jm.RunID)
	require.Contains(t, m, "q1")
	assert.Equal(t, 2, m["q1"][docOK.String()])
	_, present := m["q1"][docSkip.String()]
	assert.False(t, present, "unjudged (-1) entries must be filtered out")
}

func TestLoadJudgmentsMap_RejectsMissingSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ann.yaml")
	// Write a syntactically valid YAML but with no schema_version — the
	// judgment loader should reject it.
	require.NoError(t, os.WriteFile(path, []byte(`strategy: keyword
queries: []
`), 0644))

	_, _, err := loadJudgmentsMap(path, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema_version")
}

func TestNewRunConfig_MeasuresOneQueryAndOneEngineAtATime(t *testing.T) {
	cfg := newRunConfig(runFlags{}, &spec.BenchSpec{}, []int{10}, false)

	assert.Equal(t, runner.QueryParallelismSerial, cfg.QueryParallelism,
		"overlapping queries contend for the same engine")
	assert.Equal(t, runner.EngineParallelismSerial, cfg.EngineParallelism,
		"a zero here makes the runner fan out to every engine in the job, "+
			"so one engine's warmup overlaps another's measured iterations")
}

func TestNewRunConfig_FlagBeatsSpecDefault(t *testing.T) {
	bs := &spec.BenchSpec{}
	bs.Runs.Warmup = 1
	bs.Runs.Iterations = 3

	tests := []struct {
		name       string
		flags      runFlags
		wantWarmup int
		wantRuns   int
	}{
		{"spec defaults when flags unset", runFlags{}, 1, 3},
		{"flags win when set", runFlags{warmup: 2, iters: 10}, 2, 10},
		{"partial flags fall back per field", runFlags{iters: 10}, 1, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newRunConfig(tt.flags, bs, []int{10}, false)
			assert.Equal(t, tt.wantWarmup, cfg.WarmupRuns)
			assert.Equal(t, tt.wantRuns, cfg.Runs)
		})
	}
}

func TestNewRunConfig_SpecKValuesLoseToAnExplicitFlag(t *testing.T) {
	bs := &spec.BenchSpec{}
	bs.Metrics.KValues = []int{3, 5, 10}

	fromSpec := newRunConfig(runFlags{}, bs, []int{20}, false)
	assert.Equal(t, []int{3, 5, 10}, fromSpec.KValues, "spec k_values apply when --k was not passed")

	fromFlag := newRunConfig(runFlags{}, bs, []int{20}, true)
	assert.Equal(t, []int{20}, fromFlag.KValues, "--k must override the spec")
}

// A query-vectors package covers one suite, so --jobs narrowing a two-suite
// spec to that suite's job must run.
func TestRun_JobsFilterNarrowsTheQueryVectorsSuite(t *testing.T) {
	t.Setenv("EMBEDDING_BASE_URL", "")
	cluster := fakeCluster(t, map[string]*fakeIndex{"a": {count: 1, ranked: map[string][]uuid.UUID{"q1": {uuid.New()}}}})
	track := copyQueryVectorsTrack(t)
	require.NoError(t, os.WriteFile(filepath.Join(track, "spec.yaml"), fmt.Appendf(nil, `schema_version: 1
id: two-suites
kind: fts
engines:
  a:
    type: elasticsearch
    connection: %q
    index: a
    params:
      embedding_model: %q
runs:
  warmup: 0
  iterations: 1
jobs:
  - name: sem
    suite: suite.yaml
    engines: [a]
  - name: other
    suite: other.yaml
    engines: [a]
`, cluster, queryVectorsFixtureTag), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(track, "suite.yaml"), []byte(`schema_version: 1
id: fixture-semantic
queries:
  - id: q1
    description: "climate change policy"
    engines:
      a: { query: '{"query": {"match": {"title": "q1"}}}' }
  - id: q2
    description: "central bank interest rates"
    engines:
      a: { query: '{"query": {"match": {"title": "q2"}}}' }
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(track, "other.yaml"), []byte(`schema_version: 1
id: other
queries:
  - id: o1
    description: "something else"
    engines:
      a: { query: '{"query": {"match": {"title": "o1"}}}' }
`), 0o644))

	out, err := execute(t, newRunCmd(), track, "--jobs", "sem", "--latency-only")

	require.NoError(t, err, out)
}
