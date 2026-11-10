package pool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/engine"
)

func TestPoolResults(t *testing.T) {
	id1 := uuid.New()
	id2 := uuid.New()
	id3 := uuid.New()

	t.Run("merges and deduplicates", func(t *testing.T) {
		results := map[string]*engine.Execution{
			"engine-a": {RankedDocIDs: []uuid.UUID{id1, id2}},
			"engine-b": {RankedDocIDs: []uuid.UUID{id2, id3}},
		}

		docs := PoolResults(results, 10)
		assert.Len(t, docs, 3)

		docMap := make(map[uuid.UUID][]string)
		for _, d := range docs {
			docMap[d.DocID] = d.Sources
		}

		assert.Contains(t, docMap[id2], "engine-a")
		assert.Contains(t, docMap[id2], "engine-b")
		assert.Len(t, docMap[id1], 1)
		assert.Len(t, docMap[id3], 1)
	})

	t.Run("respects depth limit", func(t *testing.T) {
		results := map[string]*engine.Execution{
			"engine-a": {RankedDocIDs: []uuid.UUID{id1, id2, id3}},
		}

		docs := PoolResults(results, 2)
		assert.Len(t, docs, 2)
	})

	t.Run("skips nil executions", func(t *testing.T) {
		results := map[string]*engine.Execution{
			"engine-a": {RankedDocIDs: []uuid.UUID{id1}},
			"engine-b": nil,
		}

		docs := PoolResults(results, 10)
		assert.Len(t, docs, 1)
	})

	t.Run("empty results", func(t *testing.T) {
		docs := PoolResults(map[string]*engine.Execution{}, 10)
		assert.Empty(t, docs)
	})
}

// Engines are walked by name, so the same results always pool in the same
// order: docs in engine-name then rank order, sources in engine-name order.
func TestPoolResults_OrderIsFixedByEngineName(t *testing.T) {
	shared, d1, d2, d3, d4, d5 := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	results := map[string]*engine.Execution{
		"echo":    {RankedDocIDs: []uuid.UUID{d5, shared}},
		"charlie": {RankedDocIDs: []uuid.UUID{shared, d3}},
		"alpha":   {RankedDocIDs: []uuid.UUID{d1, shared}},
		"delta":   {RankedDocIDs: []uuid.UUID{d4}},
		"bravo":   {RankedDocIDs: []uuid.UUID{d2, shared}},
	}
	want := []PooledDoc{
		{DocID: d1, Sources: []string{"alpha"}},
		{DocID: shared, Sources: []string{"alpha", "bravo", "charlie", "echo"}},
		{DocID: d2, Sources: []string{"bravo"}},
		{DocID: d3, Sources: []string{"charlie"}},
		{DocID: d4, Sources: []string{"delta"}},
		{DocID: d5, Sources: []string{"echo"}},
	}

	for range 20 {
		require.Equal(t, want, PoolResults(results, 10))
	}
}

func TestPoolFileWriteRead(t *testing.T) {
	id1 := uuid.New()

	pf := &PoolFile{
		SuiteName: "test-suite",
		Queries: []PoolEntry{
			{
				QueryID:   "q1",
				QueryDesc: "test query",
				Docs: []PooledDoc{
					{DocID: id1, Sources: []string{"pg", "es"}},
				},
			},
		},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "pool.yaml")

	err := WritePoolFile(pf, path)
	require.NoError(t, err)

	_, err = os.Stat(path)
	require.NoError(t, err)

	loaded, err := ReadPoolFile(path)
	require.NoError(t, err)
	assert.Equal(t, "test-suite", loaded.SuiteName)
	assert.Len(t, loaded.Queries, 1)
	assert.Equal(t, "q1", loaded.Queries[0].QueryID)
	assert.Len(t, loaded.Queries[0].Docs, 1)
	assert.Equal(t, id1, loaded.Queries[0].Docs[0].DocID)
	assert.Equal(t, []string{"pg", "es"}, loaded.Queries[0].Docs[0].Sources)
}
