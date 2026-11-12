package judgment

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/pool"
	"github.com/tadjov/tusker/internal/types/document"

	"github.com/tadjov/tusker/internal/bench/meta"
)

type stubStorageReader struct {
	articles map[uuid.UUID]document.Article
	err      error
}

func (s *stubStorageReader) GetByIDs(_ context.Context, ids []uuid.UUID) ([]document.Article, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([]document.Article, 0, len(ids))
	for _, id := range ids {
		if a, ok := s.articles[id]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}

type fakeStrategy struct {
	name  string
	grade int
	err   error
}

func (f *fakeStrategy) Name() string { return f.name }
func (f *fakeStrategy) Describe() meta.Judge {
	return meta.Judge{Strategy: f.name}
}
func (f *fakeStrategy) Grade(_ context.Context, _ GradingQuery, _ GradingDoc) (int, error) {
	return f.grade, f.err
}

// fakeBatchStrategy simulates Anthropic-style batched grading. Configurable
// to drop docs (partial response) or fail outright.
type fakeBatchStrategy struct {
	name           string
	batchSize      int
	dropFirstN     int // number of docs in each batch to omit from the response
	batchErr       error
	perDocGrade    int
	mu             sync.Mutex
	batchCallCount int
	perDocCalls    int
}

func (f *fakeBatchStrategy) Name() string { return f.name }
func (f *fakeBatchStrategy) Describe() meta.Judge {
	return meta.Judge{Strategy: f.name}
}

func (f *fakeBatchStrategy) PreferredBatchSize() int { return f.batchSize }
func (f *fakeBatchStrategy) Grade(_ context.Context, _ GradingQuery, _ GradingDoc) (int, error) {
	f.mu.Lock()
	f.perDocCalls++
	f.mu.Unlock()
	return f.perDocGrade, nil
}
func (f *fakeBatchStrategy) GradeBatch(_ context.Context, _ GradingQuery, docs []GradingDoc) ([]GradedDoc, error) {
	f.mu.Lock()
	f.batchCallCount++
	f.mu.Unlock()

	if f.batchErr != nil {
		return nil, f.batchErr
	}
	start := f.dropFirstN
	if start > len(docs) {
		start = len(docs)
	}
	out := make([]GradedDoc, 0, len(docs)-start)
	var missing []uuid.UUID
	for i, d := range docs {
		if i < start {
			missing = append(missing, d.ID)
			continue
		}
		out = append(out, GradedDoc{DocID: d.ID, Grade: 2})
	}
	if len(missing) > 0 {
		return out, &PartialBatchError{Missing: missing, Got: len(out), Want: len(docs)}
	}
	return out, nil
}

func buildPool(t *testing.T, n int) (*pool.PoolFile, map[uuid.UUID]document.Article) {
	t.Helper()
	articles := make(map[uuid.UUID]document.Article, n)
	pe := pool.PoolEntry{QueryID: "q1", QueryText: "test"}
	for i := 0; i < n; i++ {
		id := uuid.New()
		articles[id] = document.Article{ID: id, Title: "title"}
		pe.Docs = append(pe.Docs, pool.PooledDoc{DocID: id, Sources: []string{"pg"}})
	}
	return &pool.PoolFile{SuiteName: "t", Queries: []pool.PoolEntry{pe}}, articles
}

func TestRunner_BatchedHappyPath(t *testing.T) {
	pf, articles := buildPool(t, 25)
	bs := &fakeBatchStrategy{name: "fake-batch", batchSize: 10}

	r := NewRunner(RunnerConfig{
		Strategy: bs,
		Reader:   &stubStorageReader{articles: articles},
	})
	jf, err := r.Run(context.Background(), pf)
	require.NoError(t, err)
	require.Len(t, jf.Queries[0].Docs, 25)

	// 25 docs / batch 10 → 3 batches (10 + 10 + 5)
	assert.Equal(t, 3, bs.batchCallCount)
	assert.Equal(t, 0, bs.perDocCalls)
}

func TestRunner_BatchedPartialFallsBackPerDoc(t *testing.T) {
	pf, articles := buildPool(t, 10)
	// Drop the first 2 docs of each batch → 2 missing per call → 2 per-doc retries
	bs := &fakeBatchStrategy{
		name: "fake-batch", batchSize: 10, dropFirstN: 2, perDocGrade: 1,
	}

	r := NewRunner(RunnerConfig{
		Strategy: bs,
		Reader:   &stubStorageReader{articles: articles},
	})
	jf, err := r.Run(context.Background(), pf)
	require.NoError(t, err)
	require.Len(t, jf.Queries[0].Docs, 10)
	assert.Equal(t, 1, bs.batchCallCount)
	assert.Equal(t, 2, bs.perDocCalls, "missing docs should be retried per-doc")

	// First 2 docs should have the per-doc grade (1); rest the batch grade (2)
	gradeCounts := map[int]int{}
	for _, d := range jf.Queries[0].Docs {
		gradeCounts[d.Grade]++
	}
	assert.Equal(t, 2, gradeCounts[1], "2 per-doc fallback grades")
	assert.Equal(t, 8, gradeCounts[2], "8 batch grades")
}

func TestRunner_BatchHardFailureFallsBackPerDoc(t *testing.T) {
	pf, articles := buildPool(t, 5)
	bs := &fakeBatchStrategy{
		name:        "fake-batch",
		batchSize:   10,
		batchErr:    errors.New("kaboom"),
		perDocGrade: 3,
	}
	r := NewRunner(RunnerConfig{Strategy: bs, Reader: &stubStorageReader{articles: articles}})
	jf, err := r.Run(context.Background(), pf)
	require.NoError(t, err)
	assert.Equal(t, 5, bs.perDocCalls)
	for _, d := range jf.Queries[0].Docs {
		assert.Equal(t, 3, d.Grade)
	}
}

func TestRunner_ResumeSkipsAlreadyGraded(t *testing.T) {
	pf, articles := buildPool(t, 5)
	bs := &fakeBatchStrategy{name: "fake-batch", batchSize: 10}

	// Pre-grade the first 3 docs
	prior := &File{
		Meta: meta.Meta{Judge: &meta.Judge{Strategy: "fake-batch"}},
		Queries: []Entry{
			{
				QueryID: "q1",
				Docs: []GradedDoc{
					{DocID: pf.Queries[0].Docs[0].DocID, Grade: 3},
					{DocID: pf.Queries[0].Docs[1].DocID, Grade: 3},
					{DocID: pf.Queries[0].Docs[2].DocID, Grade: 3},
				},
			},
		},
	}

	r := NewRunner(RunnerConfig{
		Strategy: bs,
		Reader:   &stubStorageReader{articles: articles},
		Existing: prior,
	})
	jf, err := r.Run(context.Background(), pf)
	require.NoError(t, err)
	require.Len(t, jf.Queries[0].Docs, 5)

	// The 3 prior grades survive, the new 2 come from the batch strategy (grade=2)
	assert.Equal(t, 3, jf.Queries[0].Docs[0].Grade)
	assert.Equal(t, 3, jf.Queries[0].Docs[1].Grade)
	assert.Equal(t, 3, jf.Queries[0].Docs[2].Grade)
	assert.Equal(t, 2, jf.Queries[0].Docs[3].Grade)
	assert.Equal(t, 2, jf.Queries[0].Docs[4].Grade)
	assert.Equal(t, 1, bs.batchCallCount, "only one batch for the 2 remaining docs")
}

func TestRunner_PerDocPathStillWorks(t *testing.T) {
	pf, articles := buildPool(t, 3)
	r := NewRunner(RunnerConfig{
		Strategy: &fakeStrategy{name: "fake", grade: 1},
		Reader:   &stubStorageReader{articles: articles},
	})
	jf, err := r.Run(context.Background(), pf)
	require.NoError(t, err)
	for _, d := range jf.Queries[0].Docs {
		assert.Equal(t, 1, d.Grade)
	}
}

func TestRunner_MissingArticleStaysUnjudged(t *testing.T) {
	pf, articles := buildPool(t, 3)
	// Remove one article from the reader's view
	missingID := pf.Queries[0].Docs[1].DocID
	delete(articles, missingID)

	r := NewRunner(RunnerConfig{
		Strategy: &fakeBatchStrategy{name: "fake-batch", batchSize: 10},
		Reader:   &stubStorageReader{articles: articles},
	})
	jf, err := r.Run(context.Background(), pf)
	require.NoError(t, err)

	for _, d := range jf.Queries[0].Docs {
		if d.DocID == missingID {
			assert.Equal(t, GradeUnjudged, d.Grade)
		}
	}
}

func TestRunner_SinkCalledPerQuery(t *testing.T) {
	pf, articles := buildPool(t, 2)
	bs := &fakeBatchStrategy{name: "fake-batch", batchSize: 10}

	calls := 0
	r := NewRunner(RunnerConfig{
		Strategy: bs,
		Reader:   &stubStorageReader{articles: articles},
		Sink: func(qp QueryProgress, _ Entry) error {
			assert.Equal(t, "q1", qp.QueryID)
			calls++
			return nil
		},
	})
	_, err := r.Run(context.Background(), pf)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestRunner_NilStrategyErrors(t *testing.T) {
	r := NewRunner(RunnerConfig{Reader: &stubStorageReader{}})
	_, err := r.Run(context.Background(), &pool.PoolFile{})
	assert.ErrorContains(t, err, "strategy is nil")
}

// A query with no usable terms fails the run, but only after every other query
// is graded, so the file still lands and names what is missing.
func TestRunner_QueryWithoutTermsFailsTheRunAfterGradingTheRest(t *testing.T) {
	for _, strategy := range []Strategy{NewKeywordStrategy(), NewBM25Strategy()} {
		t.Run(strategy.Name(), func(t *testing.T) {
			pf, articles := buildPool(t, 3)
			pf.Queries[0].QueryText = "the of a"
			usable := pool.PoolEntry{QueryID: "q2", QueryText: "title"}
			for id := range articles {
				usable.Docs = append(usable.Docs, pool.PooledDoc{DocID: id})
			}
			pf.Queries = append(pf.Queries, usable)

			sunk := map[string]bool{}
			r := NewRunner(RunnerConfig{
				Strategy: strategy,
				Reader:   &stubStorageReader{articles: articles},
				Sink: func(_ QueryProgress, e Entry) error {
					sunk[e.QueryID] = true
					return nil
				},
			})
			jf, err := r.Run(context.Background(), pf)

			var noTerms *NoQueryTermsError
			require.ErrorAs(t, err, &noTerms)
			assert.Equal(t, "q1", noTerms.QueryID)
			require.NotNil(t, jf)
			grades := map[string][]int{}
			for _, qe := range jf.Queries {
				for _, d := range qe.Docs {
					grades[qe.QueryID] = append(grades[qe.QueryID], d.Grade)
				}
			}
			assert.Equal(t, []int{GradeUnjudged, GradeUnjudged, GradeUnjudged}, grades["q1"])
			assert.NotContains(t, grades["q2"], GradeUnjudged)
			assert.Len(t, grades["q2"], 3)
			assert.True(t, sunk["q1"] && sunk["q2"], "both queries reach the sink")
		})
	}
}

// Every judge bench judge runs through the runner grades the query's text, so
// a pool entry without one fails the run before anything is graded.
func TestRunner_PoolQueryWithoutTextFailsBeforeGrading(t *testing.T) {
	pf, articles := buildPool(t, 3)
	graded := pool.PoolEntry{QueryID: "q0", QueryText: "title", Docs: pf.Queries[0].Docs}
	pf.Queries[0].QueryText = ""
	pf.Queries[0].QueryDesc = "A query described but never written"
	pf.Queries = append([]pool.PoolEntry{graded}, pf.Queries...)
	sunk := false

	_, err := NewRunner(RunnerConfig{
		Strategy: &fakeBatchStrategy{name: "fake-batch", batchSize: 10},
		Reader:   &stubStorageReader{articles: articles},
		Sink:     func(QueryProgress, Entry) error { sunk = true; return nil },
	}).Run(context.Background(), pf)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"q1"`)
	assert.False(t, sunk, "no query may be graded once one has no text")
}

// A boolean text that does not parse is a property of its query, like one
// without terms: that query stays unjudged and the others are graded.
func TestRunner_UnparsableBooleanTextFailsTheRunAfterGradingTheRest(t *testing.T) {
	pf, articles := buildPool(t, 3)
	pf.Queries[0].QueryText = "title AND (unclosed"
	pf.Queries[0].Category = "boolean"
	usable := pool.PoolEntry{QueryID: "q2", QueryText: "title"}
	for id := range articles {
		usable.Docs = append(usable.Docs, pool.PooledDoc{DocID: id})
	}
	pf.Queries = append(pf.Queries, usable)

	jf, err := NewRunner(RunnerConfig{Strategy: NewBM25Strategy(), Reader: &stubStorageReader{articles: articles}}).Run(context.Background(), pf)

	var unparsed *BooleanTextError
	require.ErrorAs(t, err, &unparsed)
	assert.Equal(t, "q1", unparsed.QueryID)
	require.NotNil(t, jf)
	for _, qe := range jf.Queries {
		for _, d := range qe.Docs {
			if qe.QueryID == "q1" {
				assert.Equal(t, GradeUnjudged, d.Grade)
			} else {
				assert.NotEqual(t, GradeUnjudged, d.Grade)
			}
		}
	}
}
