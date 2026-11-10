package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/spec"
)

// stallingExecutor answers only after the context ends, or succeeds once
// noDeadlineFallback passes so a run with no deadline fails the test instead
// of hanging it.
type stallingExecutor struct{ name string }

const noDeadlineFallback = 5 * time.Second

func (e stallingExecutor) Execute(ctx context.Context, _ engine.Request) (*engine.Execution, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(noDeadlineFallback):
		return &engine.Execution{}, nil
	}
}

func (e stallingExecutor) Name() string { return e.name }
func (e stallingExecutor) Close() error { return nil }

func (e stallingExecutor) RegisterSearchTemplate(context.Context, engine.SearchTemplate) error {
	return nil
}

func TestRunAll_EveryEngineFailsAtTheSameQueryDeadline(t *testing.T) {
	bs := oneJobTrack(t, `queries:
  - id: q-slow
    engines:
      pg: { query: "SELECT id FROM articles" }
      es: { query: '{"query": {"match_all": {}}}' }
`, map[string]spec.Engine{"pg": postgres, "es": elasticsearch}, nil)
	bs.Runs.QueryTimeout = 50 * time.Millisecond

	cfg := DefaultConfig()
	cfg.WarmupRuns = 0
	cfg.Runs = 1
	result, err := New(cfg).RunAll(context.Background(), bs, map[string]engine.Executor{
		"pg": stallingExecutor{name: "pg"},
		"es": stallingExecutor{name: "es"},
	})

	require.NoError(t, err)
	for _, name := range []string{"pg", "es"} {
		assert.ErrorIs(t, result.Jobs[0].Results["q-slow"][name].Error, context.DeadlineExceeded, name)
	}
}

type stallingRegistrar struct{ stallingExecutor }

func (e stallingRegistrar) RegisterSearchTemplate(ctx context.Context, _ engine.SearchTemplate) error {
	_, err := e.Execute(ctx, engine.Request{})
	return err
}

func TestRunAll_StoringATemplateFailsAtTheQueryDeadline(t *testing.T) {
	bs := oneJobTrack(t, `templates:
  - id: es_match
    args: [terms]
    query: '{"query": {"match": {"title": "{{terms}}"}}}'
queries:
  - id: q1
    engines:
      es: { template: es_match, params: { terms: climate } }
`, map[string]spec.Engine{"es": elasticsearch}, nil)
	bs.Runs.QueryTimeout = 50 * time.Millisecond

	_, err := New(DefaultConfig()).RunAll(context.Background(), bs, map[string]engine.Executor{
		"es": stallingRegistrar{stallingExecutor{name: "es"}},
	})

	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

var errEngineDown = errors.New("engine down")

// failingCallExecutor fails only its failOnCall-th Execute, counting from 1.
type failingCallExecutor struct {
	name       string
	failOnCall int
	calls      int
}

func (e *failingCallExecutor) Execute(context.Context, engine.Request) (*engine.Execution, error) {
	e.calls++
	if e.calls == e.failOnCall {
		return nil, errEngineDown
	}
	return &engine.Execution{Latency: time.Millisecond}, nil
}

func (e *failingCallExecutor) Name() string { return e.name }
func (e *failingCallExecutor) Close() error { return nil }

func TestRunAll_FailedIterations(t *testing.T) {
	tests := []struct {
		name       string
		warmup     int
		failOnCall int
		wantErr    error
	}{
		{name: "a failed measured iteration fails the query", failOnCall: 2, wantErr: errEngineDown},
		{name: "a failed warmup does not", warmup: 1, failOnCall: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bs := oneJobTrack(t, `queries:
  - id: q1
    engines:
      pg: { query: "SELECT id FROM articles" }
`, map[string]spec.Engine{"pg": postgres}, nil)
			cfg := DefaultConfig()
			cfg.WarmupRuns = tt.warmup
			cfg.Runs = 3

			result, err := New(cfg).RunAll(context.Background(), bs, map[string]engine.Executor{
				"pg": &failingCallExecutor{name: "pg", failOnCall: tt.failOnCall},
			})

			require.NoError(t, err)
			got := result.Jobs[0].Results["q1"]["pg"].Error
			if tt.wantErr == nil {
				assert.NoError(t, got)
				return
			}
			assert.ErrorIs(t, got, tt.wantErr)
		})
	}
}

const suiteWithAnESGap = `queries:
  - id: q-both
    engines:
      pg: { query: "SELECT id FROM articles" }
      es: { query: '{"query": {"match_all": {}}}' }
  - id: q-pg-only
    engines:
      pg: { query: "SELECT id FROM articles" }
`

// LoadSuites is the load-time check every subcommand, validate included, runs
// before it touches an engine.
func TestLoadSuites_EngineWithoutABlockForAQuery(t *testing.T) {
	tests := []struct {
		name    string
		engines map[string]spec.Engine
		wantErr []string
	}{
		{
			name:    "rejected, naming the query and the engine",
			engines: map[string]spec.Engine{"pg": postgres, "es": elasticsearch},
			wantErr: []string{"q-pg-only", `"es"`},
		},
		{
			name: "an engine reusing another's blocks has every block",
			engines: map[string]spec.Engine{
				"pg":       postgres,
				"pg-tuned": {Type: spec.EnginePostgres, QueriesFrom: "pg"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadSuites(oneJobTrack(t, suiteWithAnESGap, tt.engines, nil))

			if len(tt.wantErr) == 0 {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range tt.wantErr {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

func TestLoadSuites_EmptyBlockCountsAsMissing(t *testing.T) {
	tests := []struct {
		name  string
		block string
		empty string
	}{
		{name: "postgres null", block: `es: { query: '{"query": {"match_all": {}}}' }`, empty: "pg: ~"},
		{name: "postgres empty map", block: `es: { query: '{"query": {"match_all": {}}}' }`, empty: "pg: {}"},
		{name: "elasticsearch null", block: `pg: { query: "SELECT id FROM articles" }`, empty: "es: ~"},
		{name: "elasticsearch empty map", block: `pg: { query: "SELECT id FROM articles" }`, empty: "es: {}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bs := oneJobTrack(t, `queries:
  - id: q-empty
    engines:
      `+tt.block+`
      `+tt.empty+`
`, map[string]spec.Engine{"pg": postgres, "es": elasticsearch}, nil)

			_, err := LoadSuites(bs)

			require.Error(t, err)
			assert.ErrorContains(t, err, "q-empty")
			assert.ErrorContains(t, err, `"`+strings.SplitN(tt.empty, ":", 2)[0]+`"`)
		})
	}
}

func TestRunAll_AGapInALaterJobRunsAndStoresNothing(t *testing.T) {
	dir := t.TempDir()
	completeSuite := filepath.Join(dir, "complete.yaml")
	require.NoError(t, os.WriteFile(completeSuite, []byte(`schema_version: 1
id: complete
templates:
  - id: es_match
    args: [terms]
    query: '{"query": {"match": {"title": "{{terms}}"}}}'
queries:
  - id: q1
    engines:
      pg: { query: "SELECT id FROM articles" }
      es: { template: es_match, params: { terms: climate } }
`), 0o644))
	gapSuite := filepath.Join(dir, "gap.yaml")
	require.NoError(t, os.WriteFile(gapSuite, []byte("schema_version: 1\nid: gap\n"+suiteWithAnESGap), 0o644))
	bs := &spec.BenchSpec{
		ID:      "two_jobs",
		Engines: map[string]spec.Engine{"pg": postgres, "es": elasticsearch},
		Jobs: []spec.Job{
			{Name: "complete", Suite: completeSuite, Engines: []string{"pg", "es"}},
			{Name: "gap", Suite: gapSuite, Engines: []string{"pg", "es"}},
		},
	}
	pg := &templateStoringRecorder{recordingExecutor: recordingExecutor{name: "pg"}}
	es := &templateStoringRecorder{recordingExecutor: recordingExecutor{name: "es"}}

	_, err := New(DefaultConfig()).RunAll(context.Background(), bs, map[string]engine.Executor{"pg": pg, "es": es})

	assert.ErrorContains(t, err, "q-pg-only")
	for _, rec := range []*templateStoringRecorder{pg, es} {
		assert.Empty(t, rec.seen, "engine %s ran a query", rec.name)
		assert.Empty(t, rec.stored, "engine %s stored a template", rec.name)
	}
}

// templateStoringRecorder is a recording executor for an engine type that
// stores search templates.
type templateStoringRecorder struct {
	recordingExecutor
	stored []string
}

func (e *templateStoringRecorder) RegisterSearchTemplate(_ context.Context, template engine.SearchTemplate) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stored = append(e.stored, template.ID)
	return nil
}

// deadlineRecorder records, for every call, when it started and the deadline
// its context carried.
type deadlineRecorder struct {
	name  string
	calls []recordedDeadline
}

type recordedDeadline struct {
	start       time.Time
	deadline    time.Time
	hasDeadline bool
}

func (e *deadlineRecorder) Execute(ctx context.Context, _ engine.Request) (*engine.Execution, error) {
	deadline, ok := ctx.Deadline()
	e.calls = append(e.calls, recordedDeadline{start: time.Now(), deadline: deadline, hasDeadline: ok})
	// Calls a millisecond apart cannot share a deadline by accident of clock resolution.
	time.Sleep(time.Millisecond)
	return &engine.Execution{}, nil
}

func (e *deadlineRecorder) Name() string { return e.name }
func (e *deadlineRecorder) Close() error { return nil }

func TestRunAll_EveryExecutionGetsItsOwnDeadline(t *testing.T) {
	const queryTimeout = time.Hour
	bs := oneJobTrack(t, `queries:
  - id: q1
    engines:
      pg: { query: "SELECT id FROM articles" }
`, map[string]spec.Engine{"pg": postgres}, nil)
	bs.Runs.QueryTimeout = queryTimeout
	cfg := DefaultConfig()
	cfg.WarmupRuns = 1
	cfg.Runs = 2
	pg := &deadlineRecorder{name: "pg"}

	_, err := New(cfg).RunAll(context.Background(), bs, map[string]engine.Executor{"pg": pg})

	require.NoError(t, err)
	require.Len(t, pg.calls, cfg.WarmupRuns+cfg.Runs)
	for i, call := range pg.calls {
		require.True(t, call.hasDeadline, "execution %d (warmup first) ran without a deadline", i)
		assert.False(t, call.deadline.After(call.start.Add(queryTimeout)), "execution %d deadline is past its query timeout", i)
		if i > 0 {
			assert.True(t, call.deadline.After(pg.calls[i-1].deadline), "execution %d reuses an earlier execution's deadline", i)
		}
	}
}

const onePostgresQuery = `queries:
  - id: q1
    engines:
      pg: { query: "SELECT id FROM articles" }
`

func TestRunAll_RejectsIterationCountsThatMeasureNothing(t *testing.T) {
	tests := []struct {
		name    string
		warmup  int
		runs    int
		wantErr string
	}{
		{name: "negative iterations", warmup: 1, runs: -1, wantErr: "iterations"},
		{name: "zero iterations", warmup: 1, runs: 0, wantErr: "iterations"},
		{name: "negative warmup", warmup: -1, runs: 3, wantErr: "warmup"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.WarmupRuns = tt.warmup
			cfg.Runs = tt.runs
			pg := &recordingExecutor{name: "pg"}

			_, err := New(cfg).RunAll(context.Background(), oneJobTrack(t, onePostgresQuery, map[string]spec.Engine{"pg": postgres}, nil),
				map[string]engine.Executor{"pg": pg})

			assert.ErrorContains(t, err, tt.wantErr)
			assert.Empty(t, pg.seen)
		})
	}
}

func TestRunJob_AQueryWithNoMeasuredIterationFails(t *testing.T) {
	bs := oneJobTrack(t, onePostgresQuery, map[string]spec.Engine{"pg": postgres}, nil)
	suites, err := LoadSuites(bs)
	require.NoError(t, err)
	dialects, err := Dialects(bs)
	require.NoError(t, err)
	cfg := DefaultConfig()
	cfg.Runs = 0

	result, err := New(cfg).RunJob(context.Background(), JobRequest{
		Track:     bs.ID,
		Job:       bs.Jobs[0],
		Suite:     suites[bs.Jobs[0].Suite],
		Executors: map[string]engine.Executor{"pg": &recordingExecutor{name: "pg"}},
		Dialects:  dialects,
	})

	require.NoError(t, err)
	assert.Error(t, result.Results["q1"]["pg"].Error)
}
