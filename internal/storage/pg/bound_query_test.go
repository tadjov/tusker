package pg_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/dialect"
	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/suite"
	"github.com/tadjov/tusker/internal/storage/pg"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

const awkwardQueryText = `Ukraine's voters don't trust the "election"`

var yamlQuoted = strings.ReplaceAll(awkwardQueryText, "'", "''")

func TestPgExecutor_RunsSuiteQueryWithApostrophesAndQuotes(t *testing.T) {
	ctx := context.Background()
	pool := newBoundQueryPool(ctx, t)
	for _, title := range []string{
		`Why Ukraine's voters don't trust the "election"`,
		"Voters in Ukraine trust the election results",
		"Election turnout falls as voters stay home",
	} {
		insertArticle(ctx, t, pool, title)
	}

	exec := engine.NewPgExecutor("pg", pool)
	loaded, err := suite.Parse([]byte(`schema_version: 1
id: bound
templates:
  - id: pg_fts
    args: [terms, rank_norm, limit]
    query: |
      SELECT id FROM articles
      WHERE search_vector @@ plainto_tsquery('english', $1)
      ORDER BY ts_rank(search_vector, plainto_tsquery('english', $1), $2::int) DESC, id
      LIMIT $3::int
queries:
  - id: q-awkward
    engines:
      engine:
        template: pg_fts
        params: { terms: '` + yamlQuoted + `', limit: 10 }
`))
	require.NoError(t, err)
	resolved, err := loaded.Suite.Queries[0].ResolveEngineQuery(suite.ResolveOptions{
		Engine:   "engine",
		Registry: loaded.Registry,
		Defaults: suite.TemplateParams{"rank_norm": "1"},
	})
	require.NoError(t, err)

	asRun := dialect.Positional{}.Request("bound", resolved)
	got, err := exec.Execute(ctx, asRun)
	require.NoError(t, err)
	require.NoError(t, exec.Validate(ctx, asRun))

	want, err := exec.Execute(ctx, engine.Request{
		Query: "SELECT id FROM articles WHERE search_vector @@ plainto_tsquery('english', $1) " +
			"ORDER BY ts_rank(search_vector, plainto_tsquery('english', $1), 1) DESC, id LIMIT 10",
		Args: []any{awkwardQueryText},
	})
	require.NoError(t, err)
	require.NotEmpty(t, want.RankedDocIDs, "the corpus holds a matching article, so an empty result proves nothing")
	assert.Equal(t, want.RankedDocIDs, got.RankedDocIDs)
}

// The query vector arrives as a bound arg; it must rank exactly as the same
// vector written into the statement as a pgvector literal.
func TestPgExecutor_RunsQueryVectorArgLikeAnInlinedVector(t *testing.T) {
	ctx := context.Background()
	pool := newBoundQueryPool(ctx, t)
	const model = "qwen3-embedding:0.6b"
	for i, title := range []string{"inflation", "elections", "storms", "markets", "vaccines"} {
		id := insertArticle(ctx, t, pool, title)
		_, err := pool.GetConn().Exec(ctx,
			"INSERT INTO article_embeddings (article_id, embedding, model_name) VALUES ($1, $2::vector, $3)",
			id, suite.FormatVector(unitVector(float64(i))), model)
		require.NoError(t, err)
	}
	queryVector := unitVector(1.2)

	exec := engine.NewPgExecutor("pg", pool)
	loaded, err := suite.Parse([]byte(`schema_version: 1
id: vector
templates:
  - id: pgvector_cosine
    args: [embedding_model, query_vector, limit]
    query: |
      SELECT article_id AS id FROM article_embeddings
      WHERE model_name = $1
      ORDER BY embedding <=> $2::vector
      LIMIT $3::int
queries:
  - id: sem-1
    engines:
      pgvector-cosine: { template: pgvector_cosine, params: { limit: 3 } }
`))
	require.NoError(t, err)
	resolved, err := loaded.Suite.Queries[0].ResolveEngineQuery(suite.ResolveOptions{
		Engine:      "pgvector-cosine",
		Registry:    loaded.Registry,
		Defaults:    suite.TemplateParams{suite.EmbeddingModelParam: model},
		QueryVector: queryVector,
	})
	require.NoError(t, err)

	got, err := exec.Execute(ctx, dialect.Positional{}.Request("vector", resolved))
	require.NoError(t, err)

	want, err := exec.Execute(ctx, engine.Request{Query: fmt.Sprintf(
		"SELECT article_id AS id FROM article_embeddings WHERE model_name = '%s' ORDER BY embedding <=> '%s'::vector LIMIT 3",
		model, suite.FormatVector(queryVector))})
	require.NoError(t, err)
	require.Len(t, want.RankedDocIDs, 3)
	assert.Equal(t, want.RankedDocIDs, got.RankedDocIDs)
}

func newBoundQueryPool(ctx context.Context, t *testing.T) *pg.ConnectionPool {
	t.Helper()
	container := pkgtesting.NewPGContainerWithCleanup(ctx, t)
	pool, err := pg.NewConnectionPool(ctx, pg.PoolConfig{ConnStr: container.ConnString})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func insertArticle(ctx context.Context, t *testing.T, pool *pg.ConnectionPool, title string) string {
	t.Helper()
	var id string
	err := pool.GetConn().QueryRow(ctx,
		"INSERT INTO articles (id, title, content, url, language) VALUES (gen_random_uuid(), $1, $1, 'http://example.test/' || md5($1), 'eng') RETURNING id::text",
		title).Scan(&id)
	require.NoError(t, err)
	return id
}

// unitVector points each article in a different direction, so cosine distance
// orders them without ties.
func unitVector(angle float64) []float32 {
	v := make([]float32, 1024)
	v[0] = float32(math.Cos(angle))
	v[1] = float32(math.Sin(angle))
	return v
}
