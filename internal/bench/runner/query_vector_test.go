package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/spec"
)

type stubVectorStore struct{ vector []float32 }

func (s stubVectorStore) QueryVector(context.Context, string) ([]float32, error) {
	return s.vector, nil
}
func (s stubVectorStore) DocVectors(context.Context, []uuid.UUID) (map[uuid.UUID][]float32, error) {
	return nil, nil
}
func (s stubVectorStore) Model() string { return "qwen3-embedding:0.6b" }

// The query vector reaches a statement only through its args, so a template
// that names the reserved arg gets the embedding in that position.
func TestRunAll_FillsQueryVectorArgFromStore(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "suite.yaml")
	require.NoError(t, os.WriteFile(suitePath, []byte(`schema_version: 1
id: vector_suite
templates:
  - id: pgvector_cosine
    args: [embedding_model, query_vector, limit]
    query: "SELECT article_id AS id FROM article_embeddings WHERE model_name = $1 ORDER BY embedding <=> $2::vector LIMIT $3::int"
queries:
  - id: sem-1
    description: "economic downturns"
    engines:
      pgvector-cosine: { template: pgvector_cosine, params: { limit: 50 } }
`), 0644))

	bs := &spec.BenchSpec{
		Engines: map[string]spec.Engine{
			"pgvector-cosine": {Type: "postgres", Params: map[string]any{"embedding_model": "qwen3-embedding:0.6b"}},
			"pgvector-exact":  {Type: "postgres", QueriesFrom: "pgvector-cosine", Params: map[string]any{"embedding_model": "qwen3-embedding:0.6b"}},
		},
		Jobs: []spec.Job{{Name: "semantic", Suite: suitePath, Engines: []string{"pgvector-cosine", "pgvector-exact"}}},
	}
	cosine := &recordingExecutor{name: "pgvector-cosine"}
	exact := &recordingExecutor{name: "pgvector-exact"}

	cfg := DefaultConfig()
	cfg.WarmupRuns = 0
	cfg.Runs = 1
	cfg.VectorStore = stubVectorStore{vector: []float32{0.25, -0.5}}
	_, err := New(cfg).RunAll(context.Background(), bs, map[string]engine.Executor{
		"pgvector-cosine": cosine,
		"pgvector-exact":  exact,
	})

	require.NoError(t, err)
	want := []any{"qwen3-embedding:0.6b", "[0.25,-0.5]", "50"}
	assert.Equal(t, want, cosine.lastArgs)
	assert.Equal(t, want, exact.lastArgs)
}
