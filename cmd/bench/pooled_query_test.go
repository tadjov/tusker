package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/pool"
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
		{name: "its category", from: "    text: first query\n", to: "    text: first query\n    category: phrase\n"},
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

func TestRun_PoolOlderThanQueryTextAsksForARepool(t *testing.T) {
	tr := templatedTrack(t)
	pf := tr.pool(t)
	for i := range pf.Queries {
		pf.Queries[i].QueryText = ""
	}
	require.NoError(t, pool.WritePoolFile(pf, tr.poolPath()))
	tr.judgeEveryPooledDoc(t, pf, 1)

	_, err := execute(t, newRunCmd(), tr.dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the pool predates query text")
	assert.Contains(t, err.Error(), "re-pool and re-judge the track")
	assert.NotContains(t, err.Error(), "another text")
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

// A run reads the suites once, before measuring, so an edit made while the
// engines answer touches neither the run nor its report.
func TestRun_SuiteEditedMidRunLeavesTheRunAsRead(t *testing.T) {
	tests := []struct {
		name  string
		suite func(current string) string
	}{
		{name: "a query's text", suite: func(current string) string {
			return strings.Replace(current, "text: first query", "text: first query, edited", 1)
		}},
		{name: "into a file that no longer loads", suite: func(string) string { return "queries: [" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := uuid.New()
			cluster, err := url.Parse(fakeCluster(t, map[string]*fakeIndex{
				"a": {count: 10, ranked: map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}}},
				"b": {count: 10, ranked: map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}}},
			}))
			require.NoError(t, err)
			var armed, fired atomic.Bool
			var suitePath string
			forward := httputil.NewSingleHostReverseProxy(cluster)
			engines := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if armed.Load() && strings.Contains(r.URL.Path, "_search") && !fired.Swap(true) {
					current, err := os.ReadFile(suitePath)
					if err == nil {
						_ = os.WriteFile(suitePath, []byte(tt.suite(string(current))), 0o644)
					}
				}
				forward.ServeHTTP(w, r)
			}))
			t.Cleanup(engines.Close)
			tr := newLineageTrack(t, engines.URL)
			suitePath = filepath.Join(tr.dir, "suite.yaml")
			tr.judgeEveryPooledDoc(t, tr.pool(t), 1)
			armed.Store(true)

			_, err = execute(t, newRunCmd(), tr.dir)

			require.NoError(t, err)
			require.True(t, fired.Load(), "the suite was never edited mid-run")
			assert.NotNil(t, tr.latestReport(t))
		})
	}
}
