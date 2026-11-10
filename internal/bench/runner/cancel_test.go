package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/spec"
)

type interruptingExecutor struct {
	recordingExecutor
	interrupt context.CancelFunc
}

func (e *interruptingExecutor) Execute(ctx context.Context, req engine.Request) (*engine.Execution, error) {
	_, _ = e.recordingExecutor.Execute(ctx, req)
	e.interrupt()
	return nil, ctx.Err()
}

// An interrupted run must fail rather than hand back a result, or the caller
// writes a report whose every query after the interrupt is an error.
func TestRunAll_InterruptedRunReturnsNoResult(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "suite.yaml")
	require.NoError(t, os.WriteFile(suitePath, []byte(`schema_version: 1
id: cancel_suite
queries:
  - id: q-climate
    engines:
      pg: { query: "climate" }
  - id: q-election
    engines:
      pg: { query: "election" }
`), 0644))

	bs := &spec.BenchSpec{
		Engines: map[string]spec.Engine{"pg": {Type: "postgres"}},
		Jobs:    []spec.Job{{Name: "serial", Suite: suitePath, Engines: []string{"pg"}}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pg := &interruptingExecutor{recordingExecutor: recordingExecutor{name: "pg"}, interrupt: cancel}

	cfg := DefaultConfig()
	cfg.WarmupRuns = 0
	cfg.Runs = 1
	cfg.QueryParallelism = QueryParallelismSerial
	result, err := New(cfg).RunAll(ctx, bs, map[string]engine.Executor{"pg": pg})

	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, result)
	assert.Len(t, slices.Compact(slices.Sorted(slices.Values(pg.seen))), 1, "no query may start after the interrupt")
}
