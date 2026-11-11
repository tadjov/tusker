package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The judges graded each query's text and filters as pooled, so a run against
// a suite that reads a query otherwise scores it against another query's grades.
func TestRun_QueryRewrittenSincePoolingFailsTheRun(t *testing.T) {
	tests := []struct {
		name     string
		from, to string
	}{
		{name: "its text", from: "    text: first query\n", to: "    text: first query, edited\n"},
		{name: "its filters", from: "    text: first query\n", to: "    text: first query\n    filters: { publisher_domain: reuters.com }\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := templatedTrack(t)
			tr.judgeEveryPooledDoc(t, tr.pool(t), 1)
			editFile(t, filepath.Join(tr.dir, "suite.yaml"), tt.from, tt.to)
			before := seedLatest(t, tr)

			_, err := execute(t, newRunCmd(), tr.dir)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "q1")
			assert.NotContains(t, err.Error(), "q2")
			assert.Contains(t, err.Error(), "re-pool and re-judge the track")
			require.Len(t, tr.reports(t), 1, "the report is kept for inspection")
			assertLatestUnchanged(t, tr, before)
		})
	}
}

func TestRun_QueryWhoseDescriptionAloneChangedRunsClean(t *testing.T) {
	tr := templatedTrack(t)
	tr.judgeEveryPooledDoc(t, tr.pool(t), 1)
	editFile(t, filepath.Join(tr.dir, "suite.yaml"), "    text: first query\n", "    text: first query\n    description: The first query, described\n")

	_, err := execute(t, newRunCmd(), tr.dir)

	require.NoError(t, err)
	assert.NotNil(t, tr.latestReport(t))
}

func TestRun_LatencyOnlySkipsThePooledQueryCheck(t *testing.T) {
	tr := templatedTrack(t)
	tr.pool(t)
	editFile(t, filepath.Join(tr.dir, "suite.yaml"), "    text: first query\n", "    text: first query, edited\n")

	_, err := execute(t, newRunCmd(), tr.dir, "--latency-only")

	require.NoError(t, err)
}
