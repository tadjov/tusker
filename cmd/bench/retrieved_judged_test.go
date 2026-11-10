package main

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A doc the pool never held, as after a corpus reload, is unjudged and scores
// as non-relevant wherever an engine ranks it within the pool depth.
func TestRun_UnjudgedRetrievedDocFailsOnlyWithinThePoolDepth(t *testing.T) {
	relevant, other, unpooled := uuid.New(), uuid.New(), uuid.New()
	tests := []struct {
		name    string
		q1Rank  []uuid.UUID
		wantErr bool
	}{
		{name: "ranked within the depth", q1Rank: []uuid.UUID{relevant, unpooled, other}, wantErr: true},
		{name: "ranked past the depth", q1Rank: []uuid.UUID{relevant, other, unpooled}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &fakeIndex{count: 10, ranked: map[string][]uuid.UUID{"q1": {relevant, other}, "q2": {relevant}}}
			cluster := fakeCluster(t, map[string]*fakeIndex{
				"a": a,
				"b": {count: 10, ranked: map[string][]uuid.UUID{"q1": {other, relevant}, "q2": {other}}},
			})
			tr := newLineageTrack(t, cluster)
			tr.judgeEveryPooledDoc(t, tr.pool(t, "--depth", "2"), 1)
			a.ranked["q1"] = tt.q1Rank
			before := seedLatest(t, tr)

			_, err := execute(t, newRunCmd(), tr.dir)

			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "q1/a")
			assert.NotContains(t, err.Error(), "q1/b")
			assertLatestUnchanged(t, tr, before)
		})
	}
}

// An interrupted judge leaves pooled docs ungraded; the advice is to finish
// judging, not to throw the finished judgments away.
func TestRun_PartlyJudgedPoolAsksOnlyToResumeJudging(t *testing.T) {
	tr, relevant, other := judgedTrackDocs(t, nil)
	tr.writeJudgments(t, tr.poolRunID(t), map[string]map[uuid.UUID]int{"q1": {relevant: 3, other: 0}})

	_, err := execute(t, newRunCmd(), tr.dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "bench judge --resume")
	assert.NotContains(t, err.Error(), "re-pool")
}
