package main

import (
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/meta"
	"github.com/tadjov/tusker/internal/bench/pool"
	"github.com/tadjov/tusker/internal/bench/runner"
)

func TestPool_FailsWhenAnEngineErrored(t *testing.T) {
	doc := uuid.New()
	cluster := fakeCluster(t, map[string]*fakeIndex{
		"a": {count: 10, ranked: map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}}},
		"b": {count: 10, ranked: map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}}, failing: map[string]bool{"q2": true}},
	})
	tr := newLineageTrack(t, cluster)

	_, err := execute(t, newPoolCmd(), tr.dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "q2")
	assert.Contains(t, err.Error(), "--allow-partial")
	_, statErr := os.Stat(tr.poolPath())
	assert.True(t, errors.Is(statErr, os.ErrNotExist), "a failed pool must not replace the track's pool")
}

func TestPool_AllowPartialRecordsWhatWasDropped(t *testing.T) {
	doc := uuid.New()
	tests := []struct {
		name        string
		failing     map[string]bool
		wantEngines []string
		wantDropped []meta.DroppedExecution
	}{
		{
			name:        "engine failing one query still contributed",
			failing:     map[string]bool{"q2": true},
			wantEngines: []string{"a", "b"},
			wantDropped: []meta.DroppedExecution{{QueryID: "q2", Engine: "b"}},
		},
		{
			name:        "engine failing every query contributed nothing",
			failing:     map[string]bool{"q1": true, "q2": true},
			wantEngines: []string{"a"},
			wantDropped: []meta.DroppedExecution{{QueryID: "q1", Engine: "b"}, {QueryID: "q2", Engine: "b"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := fakeCluster(t, map[string]*fakeIndex{
				"a": {count: 10, ranked: map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}}},
				"b": {count: 10, ranked: map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}}, failing: tt.failing},
			})
			tr := newLineageTrack(t, cluster)

			_, err := execute(t, newPoolCmd(), tr.dir, "--allow-partial")
			require.NoError(t, err)

			pf, err := pool.ReadPoolFile(tr.poolPath())
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.wantEngines, pf.Meta.Engines)
			require.Len(t, pf.Meta.DroppedExecutions, len(tt.wantDropped))
			for i, want := range tt.wantDropped {
				assert.Equal(t, want.QueryID, pf.Meta.DroppedExecutions[i].QueryID)
				assert.Equal(t, want.Engine, pf.Meta.DroppedExecutions[i].Engine)
				assert.Contains(t, pf.Meta.DroppedExecutions[i].Error, "500")
			}
		})
	}
}

// A cell is dropped only when no job got an answer for it: when two jobs run
// the same engine on the same query and one succeeds, the pool has its docs.
func TestDroppedExecutions_OneJobsSuccessCoversAnother(t *testing.T) {
	doc := uuid.New()
	failed := &runner.JobResult{
		JobName: "first", EngineNames: []string{"pg"}, QueryOrder: []string{"q1"},
		Results: map[string]map[string]runner.QueryResult{"q1": {"pg": {Error: errors.New("timeout")}}},
	}
	answered := &runner.JobResult{
		JobName: "second", EngineNames: []string{"pg"}, QueryOrder: []string{"q1"},
		Results: map[string]map[string]runner.QueryResult{"q1": {"pg": {RankedDocIDs: []uuid.UUID{doc}}}},
	}

	for _, jobs := range [][]*runner.JobResult{{failed, answered}, {answered, failed}} {
		result := &runner.BenchmarkResult{Jobs: jobs}
		assert.Empty(t, droppedExecutions(result))
		pf := buildPoolFile(result, nil, nil, 10)
		require.Len(t, pf.Queries, 1)
		require.Len(t, pf.Queries[0].Docs, 1)
		assert.Equal(t, doc, pf.Queries[0].Docs[0].DocID)
	}
}

// Twelve failed cells come out of map iteration scrambled; only a sort by
// query then engine lists them in this order.
func TestDroppedExecutions_SortedByQueryThenEngine(t *testing.T) {
	queries := []string{"q3", "q1", "q2"}
	engines := []string{"delta", "alpha", "charlie", "bravo"}
	job := &runner.JobResult{JobName: "all", EngineNames: engines, QueryOrder: queries, Results: map[string]map[string]runner.QueryResult{}}
	for _, q := range queries {
		job.Results[q] = map[string]runner.QueryResult{}
		for _, e := range engines {
			job.Results[q][e] = runner.QueryResult{Error: errors.New("timeout")}
		}
	}
	var want []string
	for _, q := range []string{"q1", "q2", "q3"} {
		for _, e := range []string{"alpha", "bravo", "charlie", "delta"} {
			want = append(want, q+"/"+e)
		}
	}

	for range 5 {
		var got []string
		for _, d := range droppedExecutions(&runner.BenchmarkResult{Jobs: []*runner.JobResult{job}}) {
			got = append(got, d.QueryID+"/"+d.Engine)
		}
		require.Equal(t, want, got)
	}
}
