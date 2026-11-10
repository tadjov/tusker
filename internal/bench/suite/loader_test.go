package suite

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validHeader = `schema_version: 1
id: test_suite
version: "1.0.0"
`

func TestParse(t *testing.T) {
	t.Run("valid suite with inline queries", func(t *testing.T) {
		yaml := validHeader + `name: test
queries:
  - id: q1
    description: basic query
    engines:
      pg-native: "SELECT id FROM articles WHERE to_tsvector('english', title) @@ plainto_tsquery('english', 'climate')"
      elasticsearch: '{"query":{"match":{"title":"climate"}}}'
    judgments: []
`
		loaded, err := Parse([]byte(yaml))
		require.NoError(t, err)
		assert.Equal(t, "test", loaded.Suite.Name)
		assert.Equal(t, "test_suite", loaded.Suite.ID)
		require.NotEmpty(t, loaded.Suite.Queries)
		assert.Equal(t, "q1", loaded.Suite.Queries[0].ID)
		assert.Contains(t, loaded.Suite.Queries[0].Engines, "pg-native")
		assert.Contains(t, loaded.Suite.Queries[0].Engines, "elasticsearch")
	})

	t.Run("string engines unmarshal as EngineQuery", func(t *testing.T) {
		yaml := validHeader + `queries:
  - id: q1
    engines:
      pg: "SELECT 1"
      es: '{"query":"test"}'
    judgments: []
`
		loaded, err := Parse([]byte(yaml))
		require.NoError(t, err)
		assert.Equal(t, "SELECT 1", loaded.Suite.Queries[0].Engines["pg"].Query)
		assert.Equal(t, `{"query":"test"}`, loaded.Suite.Queries[0].Engines["es"].Query)
	})

	t.Run("structured EngineQuery with file", func(t *testing.T) {
		yaml := validHeader + `queries:
  - id: q1
    engines:
      pg:
        file: queries/pg_search.sql
    judgments: []
`
		loaded, err := Parse([]byte(yaml))
		require.NoError(t, err)
		assert.Equal(t, "queries/pg_search.sql", loaded.Suite.Queries[0].Engines["pg"].File)
	})

	t.Run("structured EngineQuery with template", func(t *testing.T) {
		yaml := validHeader + `templates:
  - id: pg_fts
    args: [term, limit]
    query: "SELECT id FROM articles WHERE term = $1 LIMIT $2::int"
queries:
  - id: q1
    engines:
      pg:
        template: pg_fts
        params: { term: climate, limit: 100 }
    judgments: []
`
		loaded, err := Parse([]byte(yaml))
		require.NoError(t, err)
		eq := loaded.Suite.Queries[0].Engines["pg"]
		assert.Equal(t, "pg_fts", eq.Template)
		assert.Equal(t, "climate", eq.Params["term"])
	})

	t.Run("no queries", func(t *testing.T) {
		_, err := Parse([]byte(validHeader + `queries: []`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no queries")
	})

	t.Run("query missing id", func(t *testing.T) {
		yaml := validHeader + `queries:
  - description: no id
    engines:
      pg: "SELECT 1"
`
		_, err := Parse([]byte(yaml))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no id")
	})

	t.Run("query missing engines", func(t *testing.T) {
		yaml := validHeader + `queries:
  - id: q1
    engines: {}
`
		_, err := Parse([]byte(yaml))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no engines")
	})

	t.Run("engine references unknown template", func(t *testing.T) {
		yaml := validHeader + `queries:
  - id: q1
    engines:
      pg:
        template: nonexistent
        params: { term: test }
`
		_, err := Parse([]byte(yaml))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown template")
	})
}

func TestParse_RejectsMissingSchemaVersion(t *testing.T) {
	yaml := `id: test
version: "1.0"
queries:
  - id: q1
    engines:
      pg: "SELECT 1"
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing schema_version")
}

func TestParse_RejectsMissingID(t *testing.T) {
	yaml := `schema_version: 1
version: "1.0"
queries:
  - id: q1
    engines:
      pg: "SELECT 1"
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required field: id")
}

func TestParse_WithJudgments(t *testing.T) {
	docID := uuid.New()

	yaml := validHeader + `name: judged suite
queries:
  - id: q1
    description: query with judgments
    engines:
      pg-native: "SELECT id FROM articles"
    judgments:
      - doc_id: ` + docID.String() + `
        relevance: 3
`
	loaded, err := Parse([]byte(yaml))
	require.NoError(t, err)
	require.NotEmpty(t, loaded.Suite.Queries[0].Judgments)
	assert.Equal(t, docID, loaded.Suite.Queries[0].Judgments[0].DocID)
	assert.Equal(t, 3, loaded.Suite.Queries[0].Judgments[0].Relevance)
}

func TestQuery_JudgmentMap(t *testing.T) {
	id1 := uuid.New()
	id2 := uuid.New()

	q := Query{
		Judgments: []RelevanceJudgment{
			{DocID: id1, Relevance: 3},
			{DocID: id2, Relevance: 1},
		},
	}

	m := q.JudgmentMap()
	assert.Equal(t, 3, m[id1])
	assert.Equal(t, 1, m[id2])
}

func TestEngineQuery_Resolve_File(t *testing.T) {
	dir := t.TempDir()
	queryFile := filepath.Join(dir, "search.sql")
	require.NoError(t, os.WriteFile(queryFile, []byte("SELECT id FROM articles WHERE id = $1"), 0644))

	eq := EngineQuery{File: "search.sql", Args: []string{"id"}, Params: TemplateParams{"id": "42"}}
	resolved, err := eq.Resolve(ResolveOptions{SuiteDir: dir})
	require.NoError(t, err)
	assert.Equal(t, "SELECT id FROM articles WHERE id = $1", resolved.Query)
	assert.Equal(t, []any{"42"}, resolved.Args)
}

func TestLoadFromFile_FileQueryResolvesBesideTheSuite(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "search.sql"), []byte("SELECT id FROM articles WHERE id = $1"), 0644))
	suiteFile := filepath.Join(dir, "suite.yaml")
	require.NoError(t, os.WriteFile(suiteFile, []byte(validHeader+`queries:
  - id: q1
    engines:
      pg: { file: search.sql, args: [id], params: { id: "42" } }
`), 0644))

	loaded, err := LoadFromFile(suiteFile)
	require.NoError(t, err)
	resolved, err := loaded.Suite.Queries[0].ResolveEngineQuery(ResolveOptions{Engine: "pg", Registry: loaded.Registry, SuiteDir: loaded.Dir})

	require.NoError(t, err)
	assert.Equal(t, "SELECT id FROM articles WHERE id = $1", resolved.Query)
}

func TestParse_RejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name, body, key string
	}{
		{
			name: "misspelt query key",
			body: `queries:
  - id: q1
    categroy: phrase
    engines:
      pg: "SELECT 1"
`,
			key: "categroy",
		},
		{
			name: "misspelt key inside an engine block",
			body: `templates:
  - id: pg_fts
    args: [term]
    query: "SELECT id FROM articles WHERE term = $1"
queries:
  - id: q1
    engines:
      pg:
        template: pg_fts
        parmas: { term: climate }
`,
			key: "parmas",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(validHeader + tc.body))
			assert.ErrorContains(t, err, tc.key)
		})
	}
}
