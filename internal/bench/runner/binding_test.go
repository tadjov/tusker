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

type recordingExecutor struct {
	name     string
	mu       sync.Mutex
	seen     []string
	lastArgs []any
}

func (e *recordingExecutor) Execute(_ context.Context, req engine.Request) (*engine.Execution, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, req.Query)
	e.lastArgs = req.Args
	return &engine.Execution{}, nil
}

func (e *recordingExecutor) Name() string { return e.name }

func (e *recordingExecutor) Close() error { return nil }

func (e *recordingExecutor) lastQuery() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.seen) == 0 {
		return ""
	}
	return e.seen[len(e.seen)-1]
}

// An engine declaring queries_from runs the source engine's query with its own
// params: the A/B arms share one suite block and can only differ where the
// spec says they do.
func TestRunAll_AliasedEngineRunsSourceBlockWithOwnParams(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "suite.yaml")
	require.NoError(t, os.WriteFile(suitePath, []byte(`schema_version: 1
id: alias_suite
templates:
  - id: pg_idx
    args: [terms, rank_norm]
    query: "ORDER BY ts_rank(search_vector, plainto_tsquery($1), $2::int) DESC"
queries:
  - id: qs-climate
    engines:
      pg-gin:
        template: pg_idx
        params: { terms: "climate change" }
`), 0644))

	bs := &spec.BenchSpec{
		Engines: map[string]spec.Engine{
			"pg-gin":      {Type: "postgres", Params: map[string]any{"rank_norm": "0"}},
			"pg-gin-norm": {Type: "postgres", QueriesFrom: "pg-gin", Params: map[string]any{"rank_norm": "1"}},
		},
		Jobs: []spec.Job{{
			Name:    "rank-ab",
			Suite:   suitePath,
			Engines: []string{"pg-gin", "pg-gin-norm"},
		}},
	}

	gin := &recordingExecutor{name: "pg-gin"}
	norm := &recordingExecutor{name: "pg-gin-norm"}

	cfg := DefaultConfig()
	cfg.WarmupRuns = 0
	cfg.Runs = 1
	_, err := New(cfg).RunAll(context.Background(), bs, map[string]engine.Executor{
		"pg-gin":      gin,
		"pg-gin-norm": norm,
	})
	require.NoError(t, err)

	assert.Equal(t, gin.lastQuery(), norm.lastQuery())
	assert.Equal(t, []any{"climate change", "0"}, gin.lastArgs)
	assert.Equal(t, []any{"climate change", "1"}, norm.lastArgs)
}

// A job whose engine is not given an arg its queries take fails before any
// job runs, not once the run reaches it.
func TestRunAll_UnsuppliedArgFailsBeforeAnyQueryRuns(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "suite.yaml")
	require.NoError(t, os.WriteFile(suitePath, []byte(`schema_version: 1
id: unsupplied_suite
templates:
  - id: pg_idx
    args: [terms, rank_norm]
    query: "SELECT id FROM articles ORDER BY ts_rank(search_vector, plainto_tsquery($1), $2::int) DESC"
queries:
  - id: qs-climate
    engines:
      pg-gin:
        template: pg_idx
        params: { terms: "climate change" }
`), 0644))

	bs := &spec.BenchSpec{
		Engines: map[string]spec.Engine{
			"pg-gin":    {Type: "postgres", Params: map[string]any{"rank_norm": "0"}},
			"pg-seq":    {Type: "postgres", QueriesFrom: "pg-gin"},
			"pg-gin-ok": {Type: "postgres", QueriesFrom: "pg-gin", Params: map[string]any{"rank_norm": "1"}},
		},
		Jobs: []spec.Job{
			{Name: "first", Suite: suitePath, Engines: []string{"pg-gin", "pg-gin-ok"}},
			{Name: "second", Suite: suitePath, Engines: []string{"pg-seq"}},
		},
	}
	gin := &recordingExecutor{name: "pg-gin"}

	cfg := DefaultConfig()
	cfg.WarmupRuns = 0
	cfg.Runs = 1
	_, err := New(cfg).RunAll(context.Background(), bs, map[string]engine.Executor{
		"pg-gin":    gin,
		"pg-gin-ok": &recordingExecutor{name: "pg-gin-ok"},
		"pg-seq":    &recordingExecutor{name: "pg-seq"},
	})

	require.Error(t, err)
	assert.ErrorContains(t, err, "pg-seq")
	assert.ErrorContains(t, err, "rank_norm")
	assert.Empty(t, gin.lastQuery(), "the first job must not run once the second is known to be broken")
}
