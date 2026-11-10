package runner

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/spec"
	"github.com/tadjov/tusker/internal/storage"
)

// fixedModelStore embeds nothing — only the model it reports is under test.
type fixedModelStore struct{ model string }

func (s fixedModelStore) QueryVector(context.Context, string) ([]float32, error) { return nil, nil }
func (s fixedModelStore) DocVectors(context.Context, []uuid.UUID) (map[uuid.UUID][]float32, error) {
	return nil, nil
}
func (s fixedModelStore) Model() string { return s.model }

func engineDeclaring(model string) spec.Engine {
	eng := spec.Engine{Type: "postgres", Connection: "postgresql://localhost/news_db"}
	if model != "" {
		eng.Params = map[string]any{"embedding_model": model}
	}
	return eng
}

func TestVerifyEmbeddingModel(t *testing.T) {
	const loaded = "qwen3-embedding:0.6b"

	tests := []struct {
		name    string
		engines map[string]spec.Engine
		store   storage.VectorStore
		wantErr bool
	}{
		{
			name:    "declaration agrees with the query embedder",
			engines: map[string]spec.Engine{"pgvector-cosine": engineDeclaring(loaded)},
			store:   fixedModelStore{model: loaded},
		},
		{
			name:    "declaration disagrees with the query embedder",
			engines: map[string]spec.Engine{"pgvector-cosine": engineDeclaring("nomic-embed-text")},
			store:   fixedModelStore{model: loaded},
			wantErr: true,
		},
		{
			name: "one arm of several disagrees",
			engines: map[string]spec.Engine{
				"pgvector-cosine": engineDeclaring(loaded),
				"pgvector-exact":  engineDeclaring("nomic-embed-text"),
				"elasticsearch":   engineDeclaring(loaded),
			},
			store:   fixedModelStore{model: loaded},
			wantErr: true,
		},
		{
			name:    "engine declares no model",
			engines: map[string]spec.Engine{"pg-gin": engineDeclaring("")},
			store:   fixedModelStore{model: loaded},
		},
		{
			name:    "no store, so nothing embeds a query",
			engines: map[string]spec.Engine{"pg-gin": engineDeclaring("nomic-embed-text")},
			store:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyEmbeddingModel(tt.engines, tt.store)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "nomic-embed-text")
			assert.Contains(t, err.Error(), loaded)
		})
	}
}

// A mismatch must stop the run before any engine is touched: the numbers such a
// run produces are valid arithmetic over two vector spaces and read as ordinary
// results.
func TestRunAllRejectsMismatchedEmbeddingModel(t *testing.T) {
	bs := &spec.BenchSpec{
		Kind:    spec.KindSemantic,
		Engines: map[string]spec.Engine{"pgvector-cosine": engineDeclaring("nomic-embed-text")},
		Jobs: []spec.Job{{
			Name:    "semantic",
			Suite:   "/nonexistent/suite.yaml",
			Engines: []string{"pgvector-cosine"},
		}},
	}
	r := New(Config{Runs: DefaultRuns, VectorStore: fixedModelStore{model: "qwen3-embedding:0.6b"}})

	result, err := r.RunAll(context.Background(), bs, nil)

	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "pgvector-cosine")
}
