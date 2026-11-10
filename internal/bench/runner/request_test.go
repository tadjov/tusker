package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/spec"
)

func singleRunConfig() Config {
	cfg := DefaultConfig()
	cfg.WarmupRuns = 0
	cfg.Runs = 1
	return cfg
}

func TestRunAll_BindsPostgresArgsInDeclaredOrder(t *testing.T) {
	bs := oneJobTrack(t, `templates:
  - id: pg_idx
    args: [limit, terms, rank_norm, published_after, weight]
    query: "SELECT id FROM articles WHERE search_vector @@ plainto_tsquery($2) AND ($4::timestamptz IS NULL OR published_at > $4) ORDER BY ts_rank(search_vector, plainto_tsquery($2), $3::int) * $5::float DESC LIMIT $1::int"
queries:
  - id: q1
    engines:
      pg: { template: pg_idx, params: { terms: "Ukraine's voters don't trust the \"election\"", limit: 100, published_after: null, weight: 1.5 } }
`, map[string]spec.Engine{"pg": {Type: spec.EnginePostgres, Params: map[string]any{"rank_norm": "1"}}}, nil)
	pg := &recordingExecutor{name: "pg"}

	_, err := New(singleRunConfig()).RunAll(context.Background(), bs, map[string]engine.Executor{"pg": pg})

	require.NoError(t, err)
	assert.Equal(t, []any{"100", `Ukraine's voters don't trust the "election"`, "1", nil, "1.5"}, pg.lastArgs,
		"values arrive as text in args order, and a null param binds as NULL")
	assert.Contains(t, pg.lastQuery(), "LIMIT $1::int", "the statement reaches the engine as written")
}

func TestRunAll_SendsQueriesOutsideTemplatesAsWritten(t *testing.T) {
	const esBody = `{"query": {"match": {"title": "Ukraine's voters"}}}`
	const apiDescriptor = `{"method": "GET", "path": "/v1/articles/search", "params": {"query": "Ukraine's voters"}}`
	bs := oneJobTrack(t, `queries:
  - id: q1
    engines:
      es: '`+strings.ReplaceAll(esBody, "'", "''")+`'
      api: '`+strings.ReplaceAll(apiDescriptor, "'", "''")+`'
`, map[string]spec.Engine{"es": elasticsearch, "api": api}, nil)
	es := &templateStoringExecutor{recordingExecutor: recordingExecutor{name: "es"}}
	apiExec := &recordingExecutor{name: "api"}

	_, err := New(singleRunConfig()).RunAll(context.Background(), bs, map[string]engine.Executor{"es": es, "api": apiExec})

	require.NoError(t, err)
	require.NotEmpty(t, es.requests)
	assert.Equal(t, engine.Request{Query: esBody}, es.requests[len(es.requests)-1])
	assert.Empty(t, es.stored)
	assert.Equal(t, apiDescriptor, apiExec.lastQuery())
	assert.Empty(t, apiExec.lastArgs)
}

func TestRunAll_RejectsATemplateEngineThatCannotStoreTemplates(t *testing.T) {
	bs := oneJobTrack(t, `templates:
  - id: es_match
    args: [terms]
    query: '{"query": {"match": {"title": "{{terms}}"}}}'
queries:
  - id: q1
    engines:
      es: { template: es_match, params: { terms: climate } }
`, map[string]spec.Engine{"es": elasticsearch}, nil)
	plain := &recordingExecutor{name: "es"}

	_, err := New(singleRunConfig()).RunAll(context.Background(), bs, map[string]engine.Executor{"es": plain})

	require.Error(t, err)
	assert.ErrorContains(t, err, "es")
	assert.Empty(t, plain.lastQuery(), "no query may run against an engine that cannot render its template")
}
