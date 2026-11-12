package runner

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/spec"
)

// templateStoringExecutor stands in for Elasticsearch: it keeps what was
// stored and records each request it receives.
type templateStoringExecutor struct {
	recordingExecutor
	storeMu  sync.Mutex
	stored   map[string]string
	requests []engine.Request
}

func (e *templateStoringExecutor) RegisterSearchTemplate(_ context.Context, template engine.SearchTemplate) error {
	e.storeMu.Lock()
	defer e.storeMu.Unlock()
	if e.stored == nil {
		e.stored = map[string]string{}
	}
	e.stored[template.ID] = template.Source
	return nil
}

func (e *templateStoringExecutor) Execute(ctx context.Context, req engine.Request) (*engine.Execution, error) {
	e.storeMu.Lock()
	e.requests = append(e.requests, req)
	e.storeMu.Unlock()
	return e.recordingExecutor.Execute(ctx, req)
}

const hybridSuite = `schema_version: 1
id: hybrid_suite
templates:
  - id: es_hybrid
    args: [terms, query_vector, size]
    query: '{"query": {"match": {"title": "{{terms}}"}}, "knn": {"field": "embedding", "query_vector": {{#toJson}}query_vector{{/toJson}}, "k": {{size}}}}'
  - id: pg_rrf
    args: [terms, query_vector]
    query: "SELECT id FROM articles WHERE search_vector @@ plainto_tsquery($1) ORDER BY $2::vector IS NULL"
queries:
  - id: hybrid-1
    text: "climate policy"
    engines:
      elasticsearch: { template: es_hybrid, params: { terms: "don't", size: 50 } }
      pg-rrf: { template: pg_rrf, params: { terms: "don't" } }
`

func TestRunAll_RunsSearchTemplatesStoredUnderTheTrack(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "suite.yaml")
	require.NoError(t, os.WriteFile(suitePath, []byte(hybridSuite), 0644))

	bs := &spec.BenchSpec{
		ID: "news_hybrid",
		Engines: map[string]spec.Engine{
			"elasticsearch": {Type: "elasticsearch"},
			"es-alias":      {Type: "elasticsearch", QueriesFrom: "elasticsearch"},
			"pg-rrf":        {Type: "postgres"},
		},
		Jobs: []spec.Job{{Name: "hybrid", Suite: suitePath, Engines: []string{"es-alias", "pg-rrf"}}},
	}
	es := &templateStoringExecutor{recordingExecutor: recordingExecutor{name: "es-alias"}}
	pg := &recordingExecutor{name: "pg-rrf"}

	cfg := DefaultConfig()
	cfg.WarmupRuns = 0
	cfg.Runs = 1
	cfg.VectorStore = stubVectorStore{vector: []float32{0.25, -0.5}}
	_, err := New(cfg).RunAll(context.Background(), bs, map[string]engine.Executor{"es-alias": es, "pg-rrf": pg})

	require.NoError(t, err)
	require.Contains(t, es.stored, "news_hybrid-es_hybrid")
	assert.NotContains(t, es.stored, "news_hybrid-pg_rrf", "a template only postgres reads stays out of Elasticsearch")
	require.NotEmpty(t, es.requests)
	last := es.requests[len(es.requests)-1]
	assert.Equal(t, "news_hybrid-es_hybrid", last.SearchTemplateID)
	assert.Equal(t, "don't", last.Params["terms"])
	assert.Equal(t, 50, last.Params["size"])
	assert.Equal(t, []float32{0.25, -0.5}, last.Params["query_vector"])
	assert.Equal(t, []any{"don't", "[0.25,-0.5]"}, pg.lastArgs)
}

func TestRunAll_RejectsTwoTemplatesStoredUnderOneID(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.yaml")
	second := filepath.Join(dir, "second.yaml")
	writeMatchSuite := func(path, field string) {
		require.NoError(t, os.WriteFile(path, []byte(`schema_version: 1
id: `+filepath.Base(path)+`
templates:
  - id: es_match
    args: [terms]
    query: '{"query": {"match": {"`+field+`": "{{terms}}"}}}'
queries:
  - id: q1
    engines:
      elasticsearch: { template: es_match, params: { terms: climate } }
`), 0644))
	}
	writeMatchSuite(first, "title")
	writeMatchSuite(second, "content")

	bs := &spec.BenchSpec{
		ID:      "fts",
		Engines: map[string]spec.Engine{"elasticsearch": {Type: "elasticsearch"}},
		Jobs: []spec.Job{
			{Name: "titles", Suite: first, Engines: []string{"elasticsearch"}},
			{Name: "bodies", Suite: second, Engines: []string{"elasticsearch"}},
		},
	}
	es := &templateStoringExecutor{recordingExecutor: recordingExecutor{name: "elasticsearch"}}

	_, err := New(DefaultConfig()).RunAll(context.Background(), bs, map[string]engine.Executor{"elasticsearch": es})

	require.Error(t, err)
	assert.ErrorContains(t, err, "fts-es_match")
	assert.Empty(t, es.stored, "a collision is found before anything is stored")
	assert.Empty(t, es.requests)
}
