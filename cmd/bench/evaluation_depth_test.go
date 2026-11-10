package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DjordjeVuckovic/tusker/internal/bench/report"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// depthTrack is pooled 3 deep while engine a ranks five judged docs for q1:
// relevant, non-relevant, relevant, relevant, non-relevant. The third relevant
// doc reaches the pool through engine b.
func depthTrack(t *testing.T) lineageTrack {
	t.Helper()
	rel1, non1, rel2, rel3, non2 := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cluster := fakeCluster(t, map[string]*fakeIndex{
		"a": {count: 10, ranked: map[string][]uuid.UUID{"q1": {rel1, non1, rel2, rel3, non2}, "q2": {rel2}}},
		"b": {count: 10, ranked: map[string][]uuid.UUID{"q1": {rel3, non2}, "q2": {rel2}}},
	})
	tr := newLineageTrack(t, cluster)
	tr.writeJudgments(t, tr.pool(t, "--depth", "3").Meta.RunID, map[string]map[uuid.UUID]int{
		"q1": {rel1: 2, non1: 0, rel2: 1, rel3: 3, non2: 0},
		"q2": {rel2: 1},
	})
	return tr
}

func perQuery(t *testing.T, rpt *report.Report, job, queryID, engine string) report.Entry {
	t.Helper()
	for _, jr := range rpt.Jobs {
		if jr.JobName != job {
			continue
		}
		for _, e := range jr.PerQuery {
			if e.QueryID == queryID && e.EngineName == engine {
				return e
			}
		}
	}
	require.Failf(t, "no entry", "%s %s/%s", job, queryID, engine)
	return report.Entry{}
}

func TestRun_ScoresEachRankingAtThePoolDepth(t *testing.T) {
	tr := depthTrack(t)

	_, err := execute(t, newRunCmd(), tr.dir)
	require.NoError(t, err)

	// Top 3 of a for q1 is rel, non, rel with R = 3 and N = 2.
	// AP = (1/1 + 2/3) / 3; bpref = ((1 - 0/2) + (1 - 1/2)) / 3.
	entry := perQuery(t, tr.latestReport(t), "both", "q1", "a")
	assert.InDelta(t, 5.0/9.0, entry.AP, 1e-9)
	assert.InDelta(t, 0.5, entry.Bpref, 1e-9)
	assert.Equal(t, 5, entry.ReturnedCount, "the engine's own count survives the cut")

	_, err = execute(t, newExportCmd(), tr.dir, "--format", "run")
	require.NoError(t, err)
	runFiles, err := filepath.Glob(filepath.Join(tr.dir, "reports", "*.run", "both.a.run"))
	require.NoError(t, err)
	require.Len(t, runFiles, 1)
	data, err := os.ReadFile(runFiles[0])
	require.NoError(t, err)
	docsPerQuery := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		docsPerQuery[strings.Fields(line)[0]]++
	}
	assert.Equal(t, 3, docsPerQuery["q1"])
}

func TestRun_RejectsCutOffsTheEvaluationCannotScore(t *testing.T) {
	tests := []struct {
		name    string
		k       string
		wantErr string
	}{
		{name: "deeper than the pool", k: "1,4", wantErr: "cut-off 4"},
		{name: "repeated", k: "2,2", wantErr: "cut-off 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := depthTrack(t)

			_, err := execute(t, newRunCmd(), tr.dir, "--k", tt.k)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Empty(t, tr.reports(t), "nothing is measured at a cut-off the pool cannot score")
		})
	}
}
