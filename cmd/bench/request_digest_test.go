package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/DjordjeVuckovic/tusker/internal/bench/pool"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const templatedSuite = `schema_version: 1
id: lineage
templates:
  - id: boosted
    args: [terms, boost]
    query: '{"query": {"match": {"title": {"query": "{{terms}}", "boost": {{boost}}}}}}'
queries:
  - id: q1
    engines:
      a: { template: boosted, params: { terms: q1 } }
      b: { template: boosted, params: { terms: q1, boost: 1 } }
  - id: q2
    engines:
      a: { template: boosted, params: { terms: q2 } }
      b: { template: boosted, params: { terms: q2, boost: 1 } }
`

// templatedTrack is the lineage track with its queries sent as a stored search
// template: engine a takes boost from the spec, engine b from each query.
func templatedTrack(t *testing.T) lineageTrack {
	t.Helper()
	doc := uuid.New()
	cluster := fakeCluster(t, map[string]*fakeIndex{
		"a": {count: 10, ranked: map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}}},
		"b": {count: 10, ranked: map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}}},
	})
	tr := newLineageTrack(t, cluster)
	require.NoError(t, os.WriteFile(filepath.Join(tr.dir, "suite.yaml"), []byte(templatedSuite), 0o644))
	return tr
}

func editFile(t *testing.T, path, from, to string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	edited := bytes.Replace(data, []byte(from), []byte(to), 1)
	require.NotEqual(t, data, edited, "%q not found in %s", from, path)
	require.NoError(t, os.WriteFile(path, edited, 0o644))
}

func TestRun_RequestChangedSincePoolingFailsTheCell(t *testing.T) {
	tests := []struct {
		name        string
		edit        func(t *testing.T, tr lineageTrack)
		wantChanged []string
		wantKept    []string
	}{
		{
			name: "the template source",
			edit: func(t *testing.T, tr lineageTrack) {
				editFile(t, filepath.Join(tr.dir, "suite.yaml"), `{"match": {"title"`, `{"match": {"description"`)
			},
			wantChanged: []string{"q1/a", "q1/b", "q2/a", "q2/b"},
		},
		{
			name: "a param the spec declares",
			edit: func(t *testing.T, tr lineageTrack) {
				editFile(t, filepath.Join(tr.dir, "spec.yaml"), "boost: 2", "boost: 3")
			},
			wantChanged: []string{"q1/a", "q2/a"},
			wantKept:    []string{"q1/b", "q2/b"},
		},
		{
			name: "one query's params",
			edit: func(t *testing.T, tr lineageTrack) {
				editFile(t, filepath.Join(tr.dir, "suite.yaml"), "terms: q1, boost: 1", "terms: q1, boost: 5")
			},
			wantChanged: []string{"q1/b"},
			wantKept:    []string{"q1/a", "q2/a", "q2/b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := templatedTrack(t)
			tr.judgeEveryPooledDoc(t, tr.pool(t), 1)
			tt.edit(t, tr)
			before := seedLatest(t, tr)

			_, err := execute(t, newRunCmd(), tr.dir)

			require.Error(t, err)
			for _, cell := range tt.wantChanged {
				assert.Contains(t, err.Error(), cell)
			}
			for _, cell := range tt.wantKept {
				assert.NotContains(t, err.Error(), cell)
			}
			assert.Contains(t, err.Error(), "re-pool and re-judge the track")
			require.Len(t, tr.reports(t), 1, "the report is kept for inspection")
			assertLatestUnchanged(t, tr, before)
		})
	}
}

func TestRun_UntouchedTemplatedTrackRunsClean(t *testing.T) {
	tr := templatedTrack(t)
	tr.judgeEveryPooledDoc(t, tr.pool(t), 1)

	_, err := execute(t, newRunCmd(), tr.dir)

	require.NoError(t, err)
	assert.NotNil(t, tr.latestReport(t))
}

func TestRun_PoolWithoutRequestDigestsFailsEveryCell(t *testing.T) {
	tr := templatedTrack(t)
	pf := tr.pool(t)
	tr.judgeEveryPooledDoc(t, pf, 1)
	for i := range pf.Queries {
		pf.Queries[i].RequestDigests = nil
	}
	require.NoError(t, pool.WritePoolFile(pf, tr.poolPath()))
	before := seedLatest(t, tr)

	_, err := execute(t, newRunCmd(), tr.dir)

	require.Error(t, err)
	for _, cell := range []string{"q1/a", "q1/b", "q2/a", "q2/b"} {
		assert.Contains(t, err.Error(), cell)
	}
	assertLatestUnchanged(t, tr, before)
}

func TestRun_LatencyOnlySkipsTheRequestCheck(t *testing.T) {
	tr := templatedTrack(t)
	tr.pool(t)
	editFile(t, filepath.Join(tr.dir, "spec.yaml"), "boost: 2", "boost: 3")

	_, err := execute(t, newRunCmd(), tr.dir, "--latency-only")

	require.NoError(t, err)
}
