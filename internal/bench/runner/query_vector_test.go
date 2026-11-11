package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/spec"
	"github.com/tadjov/tusker/internal/storage"
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
    text: "economic downturns"
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

// textKeyedStore holds a vector for each query text it embedded, as a
// query-vectors package does, and has none for any other string.
type textKeyedStore map[string][]float32

func (s textKeyedStore) QueryVector(_ context.Context, text string) ([]float32, error) {
	vector, ok := s[text]
	if !ok {
		return nil, fmt.Errorf("no vector for %q", text)
	}
	return vector, nil
}
func (s textKeyedStore) DocVectors(context.Context, []uuid.UUID) (map[uuid.UUID][]float32, error) {
	return nil, nil
}
func (s textKeyedStore) Model() string { return "qwen3-embedding:0.6b" }

func vectorSuite(t *testing.T, query string) *spec.BenchSpec {
	t.Helper()
	suitePath := filepath.Join(t.TempDir(), "suite.yaml")
	require.NoError(t, os.WriteFile(suitePath, []byte(`schema_version: 1
id: vector_suite
templates:
  - id: pgvector_cosine
    args: [embedding_model, query_vector]
    query: "SELECT article_id AS id FROM article_embeddings WHERE model_name = $1 ORDER BY embedding <=> $2::vector LIMIT 10"
queries:
`+query), 0o644))
	return &spec.BenchSpec{
		Engines: map[string]spec.Engine{
			"pgvector": {Type: "postgres", Params: map[string]any{"embedding_model": "qwen3-embedding:0.6b"}},
		},
		Jobs: []spec.Job{{Name: "semantic", Suite: suitePath, Engines: []string{"pgvector"}}},
	}
}

func runVectorSuite(t *testing.T, bs *spec.BenchSpec, store storage.VectorStore) (*recordingExecutor, QueryResult) {
	t.Helper()
	exec := &recordingExecutor{name: "pgvector"}
	cfg := DefaultConfig()
	cfg.WarmupRuns = 0
	cfg.Runs = 1
	cfg.VectorStore = store
	result, err := New(cfg).RunAll(context.Background(), bs, map[string]engine.Executor{"pgvector": exec})
	require.NoError(t, err)
	return exec, result.Jobs[0].Results["sem-1"]["pgvector"]
}

// Engines and judges look the query vector up by the same string, the text,
// so the description a suite query also carries never reaches the store.
func TestRunAll_LooksTheQueryVectorUpByTheQueryText(t *testing.T) {
	bs := vectorSuite(t, `  - id: sem-1
    text: "economic downturn"
    description: "Articles about economic downturns"
    engines:
      pgvector: { template: pgvector_cosine }
`)
	store := textKeyedStore{
		"economic downturn":                 {0.25, -0.5},
		"Articles about economic downturns": {9, 9},
	}

	exec, qr := runVectorSuite(t, bs, store)

	require.NoError(t, qr.Error)
	assert.Equal(t, []any{"qwen3-embedding:0.6b", "[0.25,-0.5]"}, exec.lastArgs)
}

// An embedder serves a vector for any string, the empty one included, so a
// query without text must fail rather than search with whatever "" embeds to.
func TestRunAll_VectorQueryWithoutTextFails(t *testing.T) {
	bs := vectorSuite(t, `  - id: sem-1
    description: "Articles about economic downturns"
    engines:
      pgvector: { template: pgvector_cosine }
`)

	exec, qr := runVectorSuite(t, bs, stubVectorStore{vector: []float32{0.25, -0.5}})

	require.Error(t, qr.Error)
	assert.Nil(t, exec.lastArgs, "a query without text was sent")
}
