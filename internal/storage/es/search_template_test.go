package es_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/runner"
	"github.com/tadjov/tusker/internal/bench/spec"
	"github.com/tadjov/tusker/internal/bench/suite"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

const (
	templateIndex    = "news"
	awkwardQueryText = `Ukraine's voters don't trust the "election"`
)

var yamlQuoted = strings.ReplaceAll(awkwardQueryText, "'", "''")

// One container serves every subtest; each writes its own documents and reads
// only through queries that match them.
func TestEsSearchTemplates(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewESContainer(ctx, t)
	esRequest(t, http.MethodPut, container.Address+"/"+templateIndex, []byte(`{
		"mappings": {"properties": {
			"id": {"type": "keyword"},
			"title": {"type": "text"},
			"embedding": {"type": "dense_vector", "dims": 3, "index": true, "similarity": "cosine"}
		}}
	}`))
	exec := engine.NewEsExecutor("elasticsearch", container.Address, templateIndex)

	t.Run("query text with apostrophes and quotes runs as a stored template", func(t *testing.T) {
		for _, title := range []string{
			`Why Ukraine's voters don't trust the "election"`,
			"Voters in Ukraine trust the election results",
			"Stock markets rally on tech earnings",
		} {
			indexDocument(t, container.Address, map[string]any{"id": uuid.NewString(), "title": title})
		}

		got := runSuite(ctx, t, exec, `templates:
  - id: es_match
    args: [terms, size]
    query: '{"query": {"match": {"title": "{{terms}}"}}, "size": {{size}}}'
queries:
  - id: q-awkward
    engines:
      elasticsearch: { template: es_match, params: { terms: '`+yamlQuoted+`', size: 10 } }
`, nil)

		handWritten, err := json.Marshal(map[string]any{
			"query": map[string]any{"match": map[string]any{"title": awkwardQueryText}},
			"size":  10,
		})
		require.NoError(t, err)
		want, err := exec.Execute(ctx, engine.Request{Query: string(handWritten)})
		require.NoError(t, err)
		require.NotEmpty(t, want.RankedDocIDs, "the index holds a matching document, so an empty result proves nothing")
		assert.Equal(t, want.RankedDocIDs, got)
	})

	t.Run("knn template ranks the query vector like an inlined vector", func(t *testing.T) {
		for _, vector := range [][]float32{{1, 0, 0}, {0.8, 0.6, 0}, {0, 1, 0}, {0, 0.6, 0.8}, {0, 0, 1}} {
			indexDocument(t, container.Address, map[string]any{"id": uuid.NewString(), "title": "vector", "embedding": vector})
		}
		queryVector := []float32{0.9, 0.3, 0.1}

		got := runSuite(ctx, t, exec, `templates:
  - id: es_knn
    args: [query_vector]
    query: '{"knn": {"field": "embedding", "query_vector": {{#toJson}}query_vector{{/toJson}}, "k": 3, "num_candidates": 10}, "size": 3}'
queries:
  - id: sem-1
    description: "nearest vectors"
    engines:
      elasticsearch: { template: es_knn }
`, queryVector)

		want, err := exec.Execute(ctx, engine.Request{Query: `{"knn": {"field": "embedding", "query_vector": ` +
			suite.FormatVector(queryVector) + `, "k": 3, "num_candidates": 10}, "size": 3}`})
		require.NoError(t, err)
		require.Len(t, want.RankedDocIDs, 3)
		assert.Equal(t, want.RankedDocIDs, got)
	})

	t.Run("validate renders the stored template before checking the body", func(t *testing.T) {
		store := func(id, source string) engine.Request {
			require.NoError(t, exec.RegisterSearchTemplate(ctx, engine.SearchTemplate{ID: id, Source: source}))
			return engine.Request{SearchTemplateID: id, Params: map[string]any{"terms": awkwardQueryText}}
		}

		valid := store("validate-match", `{"query": {"match": {"title": "{{terms}}"}}}`)
		unknownQuery := store("validate-broken", `{"query": {"no_such_query": {"title": "{{terms}}"}}}`)

		require.NoError(t, exec.Validate(ctx, valid))
		assert.Error(t, exec.Validate(ctx, unknownQuery))
	})
}

// runSuite runs one suite query through the bench runner against exec and
// returns the ids it ranked.
func runSuite(ctx context.Context, t *testing.T, exec engine.Executor, suiteBody string, queryVector []float32) []uuid.UUID {
	t.Helper()
	suitePath := filepath.Join(t.TempDir(), "suite.yaml")
	require.NoError(t, os.WriteFile(suitePath, []byte("schema_version: 1\nid: es_templates\n"+suiteBody), 0o644))
	bs := &spec.BenchSpec{
		ID:      "es_templates",
		Engines: map[string]spec.Engine{"elasticsearch": {Type: "elasticsearch"}},
		Jobs:    []spec.Job{{Name: "es", Suite: suitePath, Engines: []string{"elasticsearch"}}},
	}
	cfg := runner.DefaultConfig()
	cfg.WarmupRuns = 0
	cfg.Runs = 1
	if queryVector != nil {
		cfg.VectorStore = fixedQueryVector(queryVector)
	}

	result, err := runner.New(cfg).RunAll(ctx, bs, map[string]engine.Executor{"elasticsearch": exec})

	require.NoError(t, err)
	job := result.Jobs[0]
	require.Len(t, job.QueryOrder, 1)
	qr := job.Results[job.QueryOrder[0]]["elasticsearch"]
	require.NoError(t, qr.Error)
	return qr.RankedDocIDs
}

type fixedQueryVector []float32

func (v fixedQueryVector) QueryVector(context.Context, string) ([]float32, error) { return v, nil }
func (v fixedQueryVector) DocVectors(context.Context, []uuid.UUID) (map[uuid.UUID][]float32, error) {
	return nil, nil
}
func (v fixedQueryVector) Model() string { return "" }

func indexDocument(t *testing.T, address string, doc map[string]any) {
	t.Helper()
	body, err := json.Marshal(doc)
	require.NoError(t, err)
	esRequest(t, http.MethodPost, address+"/"+templateIndex+"/_doc?refresh=true", body)
}

func esRequest(t *testing.T, method, url string, body []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Less(t, resp.StatusCode, 300, "%s %s", method, url)
}
