package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/judgment"
	"github.com/tadjov/tusker/internal/bench/meta"
	"github.com/tadjov/tusker/internal/bench/pool"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/types/document"
)

func poolWithRunID(runID string) *pool.PoolFile {
	return &pool.PoolFile{
		Meta: meta.Meta{RunID: runID},
		Queries: []pool.PoolEntry{{
			QueryID: "q1",
			Docs:    []pool.PooledDoc{{DocID: uuid.New(), Sources: []string{"pg"}}},
		}},
	}
}

func TestCheckPoolBinding(t *testing.T) {
	current := poolWithRunID("2026-10-17T10-00-00-pool-aaaaaa")

	tests := []struct {
		name          string
		judgedAgainst string
		wantErr       string
	}{
		{"same pool", current.Meta.RunID, ""},
		{"regenerated pool", "2026-10-01T10-00-00-pool-bbbbbb", "2026-10-01T10-00-00-pool-bbbbbb"},
		{"judgments predating the binding", "", "re-judge"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPoolBinding(tt.judgedAgainst, current)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestRun_RefusesJudgmentsGradedAgainstAnotherPool(t *testing.T) {
	doc := uuid.New()
	cluster := fakeCluster(t, map[string]*fakeIndex{
		"a": {count: 10, ranked: map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}}},
		"b": {count: 10, ranked: map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}}},
	})
	tr := newLineageTrack(t, cluster)
	tr.writePool(t, "2026-10-17T10-00-00-pool-new000", map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}})
	tr.writeJudgments(t, "2026-10-01T10-00-00-pool-old000", map[string]map[uuid.UUID]int{
		"q1": {doc: 2}, "q2": {doc: 2},
	})

	_, err := execute(t, newRunCmd(), tr.dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "2026-10-01T10-00-00-pool-old000")
	assert.Empty(t, tr.reports(t), "nothing is measured against judgments of another pool")
}

func TestJudgeManual_RecordsThePoolItWasMadeFrom(t *testing.T) {
	tr := newLineageTrack(t, "http://unused.invalid")
	tr.writePool(t, "2026-10-17T10-00-00-pool-aaaaaa", map[string][]uuid.UUID{"q1": {uuid.New()}})
	outPath := filepath.Join(tr.dir, "trec", "annotations.manual.yaml")

	_, err := execute(t, newJudgeCmd(), tr.dir, "--strategy", "manual", "--output", outPath)
	require.NoError(t, err)

	jf, err := judgment.ReadFile(outPath)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-17T10-00-00-pool-aaaaaa", jf.Meta.PoolRunID)
}

type poolArticles map[uuid.UUID]document.Article

func (a poolArticles) GetByIDs(_ context.Context, ids []uuid.UUID) ([]document.Article, error) {
	out := make([]document.Article, 0, len(ids))
	for _, id := range ids {
		if article, ok := a[id]; ok {
			out = append(out, article)
		}
	}
	return out, nil
}

// gradingPool is a two-query pool bm25 can grade, with the articles it reads.
func gradingPool(t *testing.T, tr lineageTrack, runID string) poolArticles {
	t.Helper()
	climate, election := uuid.New(), uuid.New()
	pf := &pool.PoolFile{
		Meta: meta.Meta{RunID: runID, Engines: []string{"a"}},
		Queries: []pool.PoolEntry{
			{QueryID: "q1", QueryText: "climate policy", Docs: []pool.PooledDoc{{DocID: climate, Sources: []string{"a"}}}},
			{QueryID: "q2", QueryText: "election results", Docs: []pool.PooledDoc{{DocID: election, Sources: []string{"a"}}}},
		},
	}
	require.NoError(t, pool.WritePoolFile(pf, tr.poolPath()))
	return poolArticles{
		climate:  {ID: climate, Title: "Climate policy talks resume", Content: "Negotiators met on climate policy."},
		election: {ID: election, Title: "Election results", Content: "Votes were counted overnight."},
	}
}

func judgeWith(t *testing.T, tr lineageTrack, articles storage.Reader, args ...string) error {
	t.Helper()
	cmd := newJudgeCmdWithReader(func(*cobra.Command, string) (storage.Reader, error) { return articles, nil })
	_, err := execute(t, cmd, append([]string{tr.dir, "--strategy", "bm25", "--output", tr.judgments}, args...)...)
	return err
}

func TestJudgeGrading_RecordsThePoolItWasMadeFrom(t *testing.T) {
	tr := newLineageTrack(t, "http://unused.invalid")
	articles := gradingPool(t, tr, "2026-10-17T10-00-00-pool-aaaaaa")

	require.NoError(t, judgeWith(t, tr, articles))

	jf, err := judgment.ReadFile(tr.judgments)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-17T10-00-00-pool-aaaaaa", jf.Meta.PoolRunID)
}

func TestJudge_ResumeRefusesAnotherPool(t *testing.T) {
	tr := newLineageTrack(t, "http://unused.invalid")
	require.NoError(t, judgeWith(t, tr, gradingPool(t, tr, "2026-10-01T10-00-00-pool-old000")))
	regenerated := gradingPool(t, tr, "2026-10-17T10-00-00-pool-new000")

	err := judgeWith(t, tr, regenerated, "--resume")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "2026-10-01T10-00-00-pool-old000")
}

func TestJudge_ResumeRefusesAnUnboundFile(t *testing.T) {
	tr := newLineageTrack(t, "http://unused.invalid")
	articles := gradingPool(t, tr, "2026-10-17T10-00-00-pool-aaaaaa")
	require.NoError(t, judgeWith(t, tr, articles))
	unbound, err := judgment.ReadFile(tr.judgments)
	require.NoError(t, err)
	unbound.Meta.PoolRunID = ""
	require.NoError(t, judgment.WriteFile(unbound, tr.judgments))

	err = judgeWith(t, tr, articles, "--resume")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "re-judge")
}
