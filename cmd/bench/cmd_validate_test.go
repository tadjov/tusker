package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/dialect"
	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/spec"
	"github.com/tadjov/tusker/internal/bench/suite"
	"github.com/tadjov/tusker/internal/storage"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

// An interrupt mid-validate must surface as the interrupt: validating the
// remaining pairs against a cancelled context records each one as a failure
// that never happened, and the glob would carry on to the next track.
func TestValidate_InterruptReportsCancellationNotFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		_, _ = w.Write([]byte(`{"valid": true}`))
	}))
	defer es.Close()

	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		writeValidateTrack(t, filepath.Join(root, "g", name), es.URL)
	}
	prevRoot := trackRoot
	trackRoot = root
	t.Cleanup(func() { trackRoot = prevRoot })

	var out bytes.Buffer
	cmd := newValidateCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"g/*"})
	err := cmd.ExecuteContext(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, out.String(), "g/b")
	assert.NotContains(t, out.String(), "failed")
}

func writeValidateTrack(t *testing.T, dir, esURL string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "trec"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "spec.yaml"), fmt.Appendf(nil, `schema_version: 1
id: validate_interrupt
engines:
  es:
    type: elasticsearch
    connection: %q
    index: articles
jobs:
  - name: es-only
    suite: suite.yaml
    engines: [es]
`, esURL), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "suite.yaml"), []byte(`schema_version: 1
id: validate_interrupt
queries:
  - id: q-climate
    engines:
      es: { query: '{"query": {"match": {"title": "climate"}}}' }
  - id: q-election
    engines:
      es: { query: '{"query": {"match": {"title": "election"}}}' }
`), 0o644))
}

type recordingValidator struct {
	query       string
	args        []any
	deadline    time.Time
	hasDeadline bool
}

func (v *recordingValidator) Execute(context.Context, engine.Request) (*engine.Execution, error) {
	return &engine.Execution{}, nil
}
func (v *recordingValidator) Name() string { return "recording" }
func (v *recordingValidator) Close() error { return nil }

func (v *recordingValidator) Validate(ctx context.Context, req engine.Request) error {
	v.query, v.args = req.Query, req.Args
	v.deadline, v.hasDeadline = ctx.Deadline()
	return nil
}

// Validate must see the query exactly as a run sends it: the SQL as written
// plus its args in declared order, engine params included.
func TestValidateOne_PassesStatementAndOrderedArgs(t *testing.T) {
	loaded, err := suite.Parse([]byte(`schema_version: 1
id: validate_bound
templates:
  - id: pg_idx
    args: [limit, terms, rank_norm]
    query: "SELECT id FROM articles WHERE search_vector @@ plainto_tsquery('english', $2) ORDER BY ts_rank(search_vector, plainto_tsquery('english', $2), $3::int) DESC LIMIT $1::int"
queries:
  - id: q-trust
    engines:
      pg: { template: pg_idx, params: { terms: "voters don't trust", limit: 10 } }
`))
	require.NoError(t, err)
	validator := &recordingValidator{}

	row := validateOne(context.Background(), validateInput{
		query:      loaded.Suite.Queries[0],
		engineName: "pg",
		binding:    spec.QueryBinding{QuerySource: "pg", Params: map[string]any{"rank_norm": "1"}},
		loaded:     loaded,
		executor:   validator,
		dialect:    dialect.Positional{},
	})

	require.Equal(t, "OK", row.status, row.detail)
	assert.Equal(t, "SELECT id FROM articles WHERE search_vector @@ plainto_tsquery('english', $2) ORDER BY ts_rank(search_vector, plainto_tsquery('english', $2), $3::int) DESC LIMIT $1::int", validator.query)
	assert.Equal(t, []any{"10", "voters don't trust", "1"}, validator.args)
}

// A binding that cannot supply an arg fails the whole command before any pair
// is validated, the way run and pool fail before any query runs.
func TestValidate_UnsuppliedArgFailsBeforeValidating(t *testing.T) {
	validated := false
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		validated = true
		_, _ = w.Write([]byte(`{"valid": true}`))
	}))
	defer es.Close()

	root := t.TempDir()
	dir := filepath.Join(root, "unsupplied")
	writeValidateTrack(t, dir, es.URL)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "suite.yaml"), []byte(`schema_version: 1
id: validate_unsupplied
templates:
  - id: es_match
    args: [terms]
    query: '{"query": {"match": {"title": "{{terms}}"}}}'
queries:
  - id: q-climate
    engines:
      es: { query: '{"query": {"match": {"title": "climate"}}}' }
  - id: q-election
    engines:
      es: { template: es_match }
`), 0o644))
	prevRoot := trackRoot
	trackRoot = root
	t.Cleanup(func() { trackRoot = prevRoot })

	var out bytes.Buffer
	cmd := newValidateCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"unsupplied"})
	err := cmd.Execute()

	require.Error(t, err)
	assert.ErrorContains(t, err, "q-election")
	assert.ErrorContains(t, err, "terms")
	assert.False(t, validated, "no pair may be validated once a binding is known to be broken")
}

// Validate is bounded by the spec's query timeout, the deadline a run gives
// every execution, rather than by a default of the engine client's own.
func TestValidateOne_ValidatesUnderTheSpecQueryTimeout(t *testing.T) {
	const queryTimeout = 2 * time.Hour
	loaded, err := suite.Parse([]byte(`schema_version: 1
id: validate_deadline
queries:
  - id: q1
    engines:
      pg: { query: "SELECT id FROM articles" }
`))
	require.NoError(t, err)
	validator := &recordingValidator{}

	before := time.Now()
	row := validateOne(context.Background(), validateInput{
		query:        loaded.Suite.Queries[0],
		engineName:   "pg",
		binding:      spec.QueryBinding{QuerySource: "pg"},
		loaded:       loaded,
		executor:     validator,
		dialect:      dialect.Positional{},
		queryTimeout: queryTimeout,
	})
	after := time.Now()

	require.Equal(t, "OK", row.status, row.detail)
	require.True(t, validator.hasDeadline, "validation ran without a deadline")
	assert.False(t, validator.deadline.Before(before.Add(queryTimeout)), "deadline is shorter than the spec's query timeout")
	assert.False(t, validator.deadline.After(after.Add(queryTimeout)), "deadline is longer than the spec's query timeout")
}

// A track carrying a query-vectors package embeds nothing: the Ollama here
// only fails.
func TestValidate_TrackWithQueryVectorsNeedsNoOllama(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewPGContainerWithCleanup(ctx, t)
	var ollamaCalls atomic.Int32
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ollamaCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(ollama.Close)
	t.Setenv("EMBEDDING_BASE_URL", ollama.URL)
	track := queryVectorsTrack(t, container.ConnString)

	out, err := execute(t, newValidateCmd(), track)

	require.NoError(t, err, out)
	assert.Zero(t, ollamaCalls.Load())
}

// textVectors holds a vector for each query text it embedded, as a
// query-vectors package does.
type textVectors map[string][]float32

func (s textVectors) QueryVector(_ context.Context, text string) ([]float32, error) {
	vector, ok := s[text]
	if !ok {
		return nil, fmt.Errorf("no vector for %q", text)
	}
	return vector, nil
}
func (s textVectors) DocVectors(context.Context, []uuid.UUID) (map[uuid.UUID][]float32, error) {
	return nil, nil
}
func (s textVectors) Model() string { return "qwen3-embedding:0.6b" }

func validateVectorQuery(t *testing.T, query string, store storage.VectorStore) (validateRow, *recordingValidator) {
	t.Helper()
	loaded, err := suite.Parse([]byte(`schema_version: 1
id: validate_vector
templates:
  - id: knn
    args: [query_vector]
    query: "SELECT article_id FROM article_embeddings ORDER BY embedding <=> $1::vector LIMIT 10"
queries:
` + query))
	require.NoError(t, err)
	validator := &recordingValidator{}
	row := validateOne(context.Background(), validateInput{
		query:        loaded.Suite.Queries[0],
		engineName:   "pg",
		binding:      spec.QueryBinding{QuerySource: "pg"},
		loaded:       loaded,
		executor:     validator,
		store:        store,
		dialect:      dialect.Positional{},
		queryTimeout: time.Minute,
	})
	return row, validator
}

func TestValidateOne_LooksTheQueryVectorUpByTheQueryText(t *testing.T) {
	row, validator := validateVectorQuery(t, `  - id: q1
    text: "economic downturn"
    description: "Articles about economic downturns"
    engines:
      pg: { template: knn }
`, textVectors{"economic downturn": {0.25, -0.5}, "Articles about economic downturns": {9, 9}})

	require.Equal(t, "OK", row.status, row.detail)
	assert.Equal(t, []any{"[0.25,-0.5]"}, validator.args)
}

func TestValidateOne_VectorQueryWithoutTextFails(t *testing.T) {
	row, validator := validateVectorQuery(t, `  - id: q1
    description: "Articles about economic downturns"
    engines:
      pg: { template: knn }
`, textVectors{"": {0.25, -0.5}, "Articles about economic downturns": {9, 9}})

	assert.NotEqual(t, "OK", row.status)
	assert.Contains(t, row.detail, "text")
	assert.Empty(t, validator.query, "a query without text was validated")
}
