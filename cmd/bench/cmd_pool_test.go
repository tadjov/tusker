package main

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/runner"
)

// Two jobs sharing a suite must produce ONE pool entry per query_id, not one
// per (job × query). Regression test for the duplicate-query pooling bug.
func TestBuildPoolFile_DedupsQueryAcrossJobs(t *testing.T) {
	id1 := uuid.New()
	id2 := uuid.New()
	id3 := uuid.New()

	mkJob := func(name string, engines []string, ranked map[string][]uuid.UUID) *runner.JobResult {
		jr := &runner.JobResult{
			JobName:     name,
			EngineNames: engines,
			QueryOrder:  []string{"q1"},
			Results:     map[string]map[string]runner.QueryResult{"q1": {}},
		}
		for _, eng := range engines {
			jr.Results["q1"][eng] = runner.QueryResult{
				QueryID:      "q1",
				EngineName:   eng,
				RankedDocIDs: ranked[eng],
			}
		}
		return jr
	}

	result := &runner.BenchmarkResult{
		Jobs: []*runner.JobResult{
			// Job A: two engines.
			mkJob("jobA", []string{"pg-gin", "es"}, map[string][]uuid.UUID{
				"pg-gin": {id1, id2},
				"es":     {id2},
			}),
			// Job B: same suite/query, adds a third engine that contributes a new doc.
			mkJob("jobB", []string{"pg-gin", "es", "pg-seq"}, map[string][]uuid.UUID{
				"pg-gin": {id1, id2},
				"es":     {id2},
				"pg-seq": {id3},
			}),
		},
	}

	pf := buildPoolFile(result, map[string]string{"q1": "desc"}, map[string]string{"q1": "keyword"}, 50)

	require.Len(t, pf.Queries, 1, "q1 should appear exactly once after merging jobs")
	assert.Equal(t, "q1", pf.Queries[0].QueryID)
	assert.Equal(t, "keyword", pf.Queries[0].Category)

	// Union across both jobs = {id1, id2, id3}, deduplicated.
	got := make(map[uuid.UUID]bool)
	for _, d := range pf.Queries[0].Docs {
		got[d.DocID] = true
	}
	assert.Len(t, got, 3, "pool should be the deduplicated union of all engines across all jobs")
	assert.True(t, got[id1] && got[id2] && got[id3])

	// SuiteName records both contributing jobs.
	assert.Contains(t, pf.SuiteName, "jobA")
	assert.Contains(t, pf.SuiteName, "jobB")
}

// Distinct query_ids across jobs are all preserved in first-seen order.
func TestBuildPoolFile_PreservesDistinctQueries(t *testing.T) {
	id := uuid.New()
	job := &runner.JobResult{
		JobName:     "job",
		EngineNames: []string{"pg"},
		QueryOrder:  []string{"q1", "q2"},
		Results: map[string]map[string]runner.QueryResult{
			"q1": {"pg": {QueryID: "q1", EngineName: "pg", RankedDocIDs: []uuid.UUID{id}}},
			"q2": {"pg": {QueryID: "q2", EngineName: "pg", RankedDocIDs: []uuid.UUID{id}}},
		},
	}
	result := &runner.BenchmarkResult{Jobs: []*runner.JobResult{job}}

	pf := buildPoolFile(result, map[string]string{"q1": "d1", "q2": "d2"}, nil, 50)

	require.Len(t, pf.Queries, 2)
	assert.Equal(t, "q1", pf.Queries[0].QueryID)
	assert.Equal(t, "q2", pf.Queries[1].QueryID)
}
