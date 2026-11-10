package judgment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/meta"
)

// widthMismatchStore embeds queries at one width and returns documents at
// another — what a store holding a different model's vectors looks like.
func widthMismatchStore(t *testing.T, pooled map[uuid.UUID]struct{}, queryWidth, docWidth int) fakeVectorStore {
	t.Helper()
	docs := make(map[uuid.UUID][]float32, len(pooled))
	for id := range pooled {
		docs[id] = make([]float32, docWidth)
	}
	return fakeVectorStore{query: make([]float32, queryWidth), docs: docs, model: "qwen3-embedding:0.6b"}
}

func TestVectorStrategy_WidthMismatchStopsTheRunAndWritesNothing(t *testing.T) {
	pf, articles := buildPool(t, 8)
	ids := make(map[uuid.UUID]struct{}, len(articles))
	for id := range articles {
		ids[id] = struct{}{}
	}

	store := widthMismatchStore(t, ids, 1024, 768)
	outPath := filepath.Join(t.TempDir(), "trec", "annotations.vector.yaml")
	writer := NewIncrementalWriter(outPath, meta.Meta{Judge: &meta.Judge{Strategy: string(StrategyVector)}})

	r := NewRunner(RunnerConfig{
		Strategy: NewVectorStrategyWithStore(store, "qwen3-embedding:0.6b"),
		Reader:   &stubStorageReader{articles: articles},
		Sink:     writer.Append,
	})

	_, err := r.Run(context.Background(), pf)

	require.Error(t, err, "a width mismatch must fail the run, not grade every document zero")
	var width *VectorWidthError
	require.True(t, errors.As(err, &width), "want a VectorWidthError, got %v", err)
	assert.Equal(t, 1024, width.QueryWidth)
	assert.Equal(t, 768, width.DocWidth)

	_, statErr := os.Stat(outPath)
	assert.True(t, os.IsNotExist(statErr), "no annotations file may be written when the run fails")
}

func TestVectorStrategy_EmptyQueryVectorStopsTheRun(t *testing.T) {
	pf, articles := buildPool(t, 3)
	ids := make(map[uuid.UUID]struct{}, len(articles))
	for id := range articles {
		ids[id] = struct{}{}
	}

	store := widthMismatchStore(t, ids, 0, 768)
	r := NewRunner(RunnerConfig{
		Strategy: NewVectorStrategyWithStore(store, "qwen3-embedding:0.6b"),
		Reader:   &stubStorageReader{articles: articles},
	})

	_, err := r.Run(context.Background(), pf)

	var width *VectorWidthError
	require.True(t, errors.As(err, &width), "want a VectorWidthError, got %v", err)
}

// The hybrid strategy shares the vector component, so a mismatch must stop it
// too rather than quietly fusing a zero vector signal with BM25.
func TestHybridStrategy_WidthMismatchStopsTheRun(t *testing.T) {
	pf, articles := buildPool(t, 5)
	ids := make(map[uuid.UUID]struct{}, len(articles))
	for id := range articles {
		ids[id] = struct{}{}
	}

	store := widthMismatchStore(t, ids, 1024, 768)
	r := NewRunner(RunnerConfig{
		Strategy: NewHybridStrategyWithStore(store, "qwen3-embedding:0.6b"),
		Reader:   &stubStorageReader{articles: articles},
	})

	_, err := r.Run(context.Background(), pf)

	var width *VectorWidthError
	require.True(t, errors.As(err, &width), "want a VectorWidthError, got %v", err)
}
