package runner

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/spec"
)

var (
	postgres      = spec.Engine{Type: spec.EnginePostgres}
	elasticsearch = spec.Engine{Type: spec.EngineElasticsearch}
	api           = spec.Engine{Type: spec.EngineAPI}
)

// oneJobTrack writes the suite and any query files to a fresh directory and
// returns a spec whose single job runs every engine against it.
func oneJobTrack(t *testing.T, suiteYAML string, engines map[string]spec.Engine, files map[string]string) *spec.BenchSpec {
	t.Helper()
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "suite.yaml")
	require.NoError(t, os.WriteFile(suitePath, []byte("schema_version: 1\nid: dialect_suite\n"+suiteYAML), 0o644))
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	return &spec.BenchSpec{
		ID:      "dialects",
		Engines: engines,
		Jobs:    []spec.Job{{Name: "all", Suite: suitePath, Engines: slices.Sorted(maps.Keys(engines))}},
	}
}

func TestLoadSuites_RejectsABlockItsEngineCannotRun(t *testing.T) {
	tests := []struct {
		name    string
		suite   string
		engines map[string]spec.Engine
		files   map[string]string
		wantErr []string
	}{
		{
			name: "postgres args stop short of the highest placeholder",
			suite: `templates:
  - id: pg_idx
    args: [terms]
    query: "SELECT id FROM articles WHERE search_vector @@ plainto_tsquery($1) LIMIT $2::int"
queries:
  - id: q1
    engines:
      pg: { template: pg_idx, params: { terms: climate } }
`,
			engines: map[string]spec.Engine{"pg": postgres},
			wantErr: []string{"q1", "pg", "$2"},
		},
		{
			name: "postgres args name more values than the statement takes",
			suite: `templates:
  - id: pg_idx
    args: [terms, rank_norm, limit]
    query: "SELECT id FROM articles WHERE search_vector @@ plainto_tsquery($1) LIMIT $2::int"
queries:
  - id: q1
    engines:
      pg: { template: pg_idx, params: { terms: climate, rank_norm: 1, limit: 10 } }
`,
			engines: map[string]spec.Engine{"pg": postgres},
			wantErr: []string{"q1", "pg", "limit"},
		},
		{
			name: "postgres statement skips a placeholder",
			suite: `queries:
  - id: q1
    engines:
      pg:
        query: "SELECT id FROM articles WHERE title = $1 OR description = $3"
        args: [terms, field, other]
        params: { terms: climate, field: title, other: x }
`,
			engines: map[string]spec.Engine{"pg": postgres},
			wantErr: []string{"q1", "pg", "$2"},
		},
		{
			name: "postgres file query args stop short",
			suite: `queries:
  - id: q1
    engines:
      pg: { file: search.sql, args: [terms], params: { terms: climate } }
`,
			engines: map[string]spec.Engine{"pg": postgres},
			files:   map[string]string{"search.sql": "SELECT id FROM articles WHERE title = $1 LIMIT $2::int"},
			wantErr: []string{"q1", "pg", "$2"},
		},
		{
			name: "postgres param holds a list one placeholder cannot bind",
			suite: `queries:
  - id: q1
    engines:
      pg:
        query: "SELECT id FROM articles WHERE title = ANY($1::text[])"
        args: [titles]
        params: { titles: [a, b] }
`,
			engines: map[string]spec.Engine{"pg": postgres},
			wantErr: []string{"q1", "pg", "titles"},
		},
		{
			name: "search template reads a name its args leave out",
			suite: `templates:
  - id: es_hybrid
    args: [terms]
    query: '{"query": {"match": {"title": "{{terms}}"}}, "knn": {"query_vector": {{#toJson}}query_vector{{/toJson}}}}'
queries:
  - id: q1
    engines:
      es: { template: es_hybrid, params: { terms: climate } }
`,
			engines: map[string]spec.Engine{"es": elasticsearch},
			wantErr: []string{"es_hybrid", "query_vector"},
		},
		{
			name: "search template args name a param the source never reads",
			suite: `templates:
  - id: es_match
    args: [terms, size]
    query: '{"query": {"match": {"title": "{{terms}}"}}}'
queries:
  - id: q1
    engines:
      es: { template: es_match, params: { terms: climate, size: 10 } }
`,
			engines: map[string]spec.Engine{"es": elasticsearch},
			wantErr: []string{"es_match", "size"},
		},
		{
			name: "inline elasticsearch query reads a name nothing renders",
			suite: `queries:
  - id: q1
    engines:
      es:
        query: '{"query": {"match": {"title": "{{terms}}"}}}'
        args: [terms]
        params: { terms: climate }
`,
			engines: map[string]spec.Engine{"es": elasticsearch},
			wantErr: []string{"q1", "es", "terms"},
		},
		{
			name: "inline elasticsearch query declares args it cannot receive",
			suite: `queries:
  - id: q1
    engines:
      es:
        query: '{"query": {"match": {"title": "climate"}}}'
        args: [terms]
        params: { terms: "don't" }
`,
			engines: map[string]spec.Engine{"es": elasticsearch},
			wantErr: []string{"q1", "es", "terms"},
		},
		{
			name: "api engine runs a template only elasticsearch renders",
			suite: `templates:
  - id: api_search
    args: [terms]
    query: '{"method": "GET", "path": "/v1/articles/search", "params": {"query": "{{terms}}"}}'
queries:
  - id: q1
    engines:
      api: { template: api_search, params: { terms: climate } }
`,
			engines: map[string]spec.Engine{"api": api},
			wantErr: []string{"q1", "api", "terms"},
		},
		{
			name: "api descriptor reads a name nothing renders",
			suite: `queries:
  - id: q1
    engines:
      api: '{"method": "GET", "path": "/v1/articles/search", "params": {"query": "{{terms}}"}}'
`,
			engines: map[string]spec.Engine{"api": api},
			wantErr: []string{"q1", "api", "terms"},
		},
		{
			name: "one template read by engines that take different syntax",
			suite: `templates:
  - id: match_all
    query: '{"query": {"match_all": {}}}'
queries:
  - id: q1
    engines:
      es: { template: match_all }
      pg: { template: match_all }
`,
			engines: map[string]spec.Engine{"es": elasticsearch, "pg": postgres},
			wantErr: []string{"match_all", "es", "pg"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bs := oneJobTrack(t, tt.suite, tt.engines, tt.files)

			_, err := LoadSuites(bs)

			require.Error(t, err)
			for _, want := range tt.wantErr {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

func TestLoadSuites_AcceptsBlocksWrittenInTheirEngineSyntax(t *testing.T) {
	tests := []struct {
		name    string
		suite   string
		engines map[string]spec.Engine
	}{
		{
			name: "postgres reads a placeholder more than once",
			suite: `templates:
  - id: pg_idx
    args: [terms, limit]
    query: "SELECT id FROM articles WHERE search_vector @@ plainto_tsquery($1) ORDER BY ts_rank(search_vector, plainto_tsquery($1)) DESC LIMIT $2::int"
queries:
  - id: q1
    engines:
      pg: { template: pg_idx, params: { terms: climate, limit: 10 } }
`,
			engines: map[string]spec.Engine{"pg": postgres},
		},
		{
			name: "dollar amount in an inline elasticsearch query",
			suite: `queries:
  - id: q1
    engines:
      es: '{"query": {"match": {"title": "bitcoin hits $100000"}}}'
`,
			engines: map[string]spec.Engine{"es": elasticsearch},
		},
		{
			name: "dollar amount in an api descriptor",
			suite: `queries:
  - id: q1
    engines:
      api: '{"method": "GET", "path": "/v1/articles/search", "params": {"query": "bitcoin $100000"}}'
`,
			engines: map[string]spec.Engine{"api": api},
		},
		{
			name: "search template joins a list with a delimiter",
			suite: `templates:
  - id: es_tags
    args: [terms, tags]
    query: '{"query": {"match": {"title": "{{terms}} {{#join delimiter=''||''}}tags{{/join delimiter=''||''}}"}}}'
queries:
  - id: q1
    engines:
      es: { template: es_tags, params: { terms: climate, tags: [policy, energy] } }
`,
			engines: map[string]spec.Engine{"es": elasticsearch},
		},
		{
			name: "search template iterates a list of objects",
			suite: `templates:
  - id: es_should
    args: [should]
    query: '{"query": {"bool": {"should": [{{#should}}{"match": {"{{field}}": "{{text}}"}},{{/should}} {"match_none": {}}]}}}'
queries:
  - id: q1
    engines:
      es: { template: es_should, params: { should: [{ field: title, text: climate }] } }
`,
			engines: map[string]spec.Engine{"es": elasticsearch},
		},
		{
			name: "search template reads a param only inside a conditional section",
			suite: `templates:
  - id: es_range
    args: [terms, from, size]
    query: '{"query": {"bool": {"must": {"match": {"title": "{{terms}}"}}{{#from}}, "filter": {"range": {"published_at": {"gte": "{{from}}"}}}}{{/from}}}}, "size": {{{size}}}}'
queries:
  - id: q1
    engines:
      es: { template: es_range, params: { terms: climate, from: "2024-01-01", size: 10 } }
`,
			engines: map[string]spec.Engine{"es": elasticsearch},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bs := oneJobTrack(t, tt.suite, tt.engines, nil)

			_, err := LoadSuites(bs)

			require.NoError(t, err)
		})
	}
}
