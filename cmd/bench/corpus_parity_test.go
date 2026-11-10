package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/engine"
)

type countedExecutor struct {
	count int64
	err   error
}

func (countedExecutor) Execute(context.Context, engine.Request) (*engine.Execution, error) {
	return &engine.Execution{}, nil
}
func (countedExecutor) Name() string                                 { return "counted" }
func (countedExecutor) Close() error                                 { return nil }
func (e countedExecutor) CorpusCount(context.Context) (int64, error) { return e.count, e.err }

func TestCollectCorpusInfo(t *testing.T) {
	tests := []struct {
		name      string
		counts    map[string]int64
		failing   string
		wantErr   string
		wantCount int64
	}{
		{name: "engines agree", counts: map[string]int64{"pg": 707910, "es": 707910}, wantCount: 707910},
		{name: "an empty engine alone", counts: map[string]int64{"es": 0}, wantErr: "es"},
		{name: "every engine empty", counts: map[string]int64{"pg": 0, "es": 0}, wantErr: "0 documents"},
		{name: "engines disagree", counts: map[string]int64{"pg": 5, "es": 7}, wantErr: "different"},
		{name: "an engine cannot be counted", counts: map[string]int64{"pg": 5, "es": 5}, failing: "es", wantErr: "connection refused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executors := make(map[string]engine.Executor, len(tt.counts))
			for name, n := range tt.counts {
				executors[name] = countedExecutor{count: n}
				if name == tt.failing {
					executors[name] = countedExecutor{err: errors.New("connection refused")}
				}
			}

			info, err := collectCorpusInfo(context.Background(), executors)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantCount, info.DocCount)
		})
	}
}

func TestRun_CorpusMismatchFailsBeforeRunning(t *testing.T) {
	doc := uuid.New()
	cluster := fakeCluster(t, map[string]*fakeIndex{
		"a": {count: 10, ranked: map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}}},
		"b": {count: 12, ranked: map[string][]uuid.UUID{"q1": {doc}, "q2": {doc}}},
	})
	tr := newLineageTrack(t, cluster)

	_, err := execute(t, newRunCmd(), tr.dir, "--latency-only")

	require.Error(t, err)
	assert.Empty(t, tr.reports(t))
}
