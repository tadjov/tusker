package judgment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/bench/meta"
	"github.com/tadjov/tusker/internal/bench/pool"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/types/document"
)

const (
	defaultConcurrency = 4
	minConcurrency     = 1
	maxConcurrency     = 32
	defaultBatchSize   = 10
)

// BatchProgress is delivered to RunnerConfig.OnBatch after each batch call.
// Lets the CLI print live progress like:
//
//	[qs-climate] batch 2/8: graded=20 (3:5 2:8 1:4 0:3) missing=0 elapsed=820ms
type BatchProgress struct {
	QueryID   string
	BatchIdx  int
	BatchN    int
	Graded    int
	Missing   int
	Histogram map[int]int
}

// QueryProgress reports a single query's outcome.
type QueryProgress struct {
	QueryID   string
	Graded    int
	Skipped   int // already-graded docs skipped via resume
	Unjudged  int // strategy returned no grade (failures)
	Histogram map[int]int
}

// RunnerConfig configures judge orchestration.
type RunnerConfig struct {
	Strategy    Strategy
	Reader      storage.Reader
	Concurrency int
	// BatchSize overrides the strategy's preferred batch size. 0 = use preferred.
	// A PoolStrategy refuses any other value.
	BatchSize int
	// Existing is an optional prior judgment file. Entries already present
	// will be skipped on this run (resume support).
	Existing *File
	// Sink, if non-nil, is invoked after every query so the caller can
	// persist incrementally. Receives an immutable snapshot of the query's
	// JudgmentEntry.
	Sink func(QueryProgress, Entry) error
	// OnQueryStart, OnQueryDone, OnBatch are optional progress callbacks.
	OnQueryStart func(queryID string, docCount int, alreadyGraded int)
	OnQueryDone  func(QueryProgress)
	OnBatch      func(BatchProgress)
}

// Runner orchestrates relevance grading. It:
//   - Fetches articles per-query in one DB round-trip
//   - Uses PoolStrategy.GradePool on the query's whole pool when supported
//   - Otherwise uses BatchStrategy.GradeBatch when supported, falling back to per-doc
//     Grade() for missing entries (Anthropic LLM-as-judge batched pattern)
//   - Skips docs already graded in cfg.Existing (resume)
//   - Streams results through Sink so callers can checkpoint to disk
type Runner struct {
	cfg RunnerConfig
}

func NewRunner(cfg RunnerConfig) *Runner {
	if cfg.Concurrency < minConcurrency {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.Concurrency > maxConcurrency {
		cfg.Concurrency = maxConcurrency
	}
	return &Runner{cfg: cfg}
}

// Run grades every pool entry and returns a complete JudgmentFile. A query with
// no usable terms leaves its docs unjudged without stopping the others: Run
// then returns the file together with an error that joins a NoQueryTermsError
// per such query.
func (r *Runner) Run(ctx context.Context, pf *pool.PoolFile) (*File, error) {
	if r.cfg.Strategy == nil {
		return nil, fmt.Errorf("runner: strategy is nil")
	}

	switch r.cfg.Strategy.(type) {
	case PoolStrategy:
		if r.cfg.BatchSize > 0 {
			return nil, fmt.Errorf("%s grades each query's whole pool at once and takes no batch size", r.cfg.Strategy.Name())
		}
	case DocStrategy:
	default:
		return nil, fmt.Errorf("runner: strategy %q implements no grading interface", r.cfg.Strategy.Name())
	}

	if err := requireQueryText(pf); err != nil {
		return nil, err
	}

	prior := indexExisting(r.cfg.Existing)

	judge := r.cfg.Strategy.Describe()
	jf := &File{
		Meta:    meta.Meta{Judge: &judge},
		Queries: make([]Entry, 0, len(pf.Queries)),
	}

	var unusable []error
	for _, entry := range pf.Queries {
		ge, prog, err := r.gradeQuery(ctx, entry, prior[entry.QueryID])
		var noTerms *NoQueryTermsError
		if errors.As(err, &noTerms) {
			unusable = append(unusable, err)
		} else if err != nil {
			return nil, fmt.Errorf("grade query %q: %w", entry.QueryID, err)
		}
		jf.Queries = append(jf.Queries, ge)

		if r.cfg.Sink != nil {
			if err := r.cfg.Sink(prog, ge); err != nil {
				return nil, fmt.Errorf("sink query %q: %w", entry.QueryID, err)
			}
		}
		if r.cfg.OnQueryDone != nil {
			r.cfg.OnQueryDone(prog)
		}
	}
	return jf, errors.Join(unusable...)
}

func requireQueryText(pf *pool.PoolFile) error {
	var missing []string
	for _, entry := range pf.Queries {
		if entry.QueryText == "" {
			missing = append(missing, strconv.Quote(entry.QueryID))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("pool queries %s have no text to grade: give each suite query its text and re-pool", strings.Join(missing, ", "))
	}
	return nil
}

func (r *Runner) gradeQuery(ctx context.Context, entry pool.PoolEntry, prior map[uuid.UUID]int) (Entry, QueryProgress, error) {
	ge := Entry{QueryID: entry.QueryID, Docs: make([]GradedDoc, 0, len(entry.Docs))}
	prog := QueryProgress{QueryID: entry.QueryID, Histogram: map[int]int{}}

	if len(entry.Docs) == 0 {
		return ge, prog, nil
	}

	var todo, kept []pool.PooledDoc
	for _, pd := range entry.Docs {
		if g, ok := prior[pd.DocID]; ok && g >= 0 {
			kept = append(kept, pd)
		} else {
			todo = append(todo, pd)
		}
	}
	// A pool grade is relative to the pool the prior run could read, so a
	// query with anything left to grade is regraded whole.
	if _, wholePool := r.cfg.Strategy.(PoolStrategy); wholePool && len(todo) > 0 {
		todo, kept = entry.Docs, nil
	}
	for _, pd := range kept {
		g := prior[pd.DocID]
		ge.Docs = append(ge.Docs, GradedDoc{DocID: pd.DocID, Grade: g})
		prog.Skipped++
		prog.Histogram[g]++
	}
	if r.cfg.OnQueryStart != nil {
		r.cfg.OnQueryStart(entry.QueryID, len(entry.Docs), prog.Skipped)
	}
	if len(todo) == 0 {
		return ge, prog, nil
	}

	q := GradingQuery{
		ID:          entry.QueryID,
		Text:        entry.QueryText,
		Description: entry.QueryDesc,
		Category:    entry.Category,
		Filters:     entry.Filters,
	}
	var gradedByID map[uuid.UUID]int
	var err error
	if ps, ok := r.cfg.Strategy.(PoolStrategy); ok {
		gradedByID, err = r.gradeWholePool(ctx, ps, q, entry.Docs)
	} else {
		gradedByID, err = r.gradeTodo(ctx, q, todo)
	}
	var noTerms *NoQueryTermsError
	if err != nil && !errors.As(err, &noTerms) {
		return ge, prog, err
	}

	// Append in original pool order — keeps doc ordering stable across runs.
	for _, pd := range todo {
		grade, ok := gradedByID[pd.DocID]
		if !ok {
			grade = GradeUnjudged
			prog.Unjudged++
		} else {
			prog.Graded++
			prog.Histogram[grade]++
		}
		ge.Docs = append(ge.Docs, GradedDoc{DocID: pd.DocID, Grade: grade})
	}
	return ge, prog, err
}

func (r *Runner) gradeTodo(ctx context.Context, q GradingQuery, todo []pool.PooledDoc) (map[uuid.UUID]int, error) {
	docs, err := r.fetchDocs(ctx, todo)
	if err != nil {
		return nil, fmt.Errorf("fetch docs: %w", err)
	}
	return r.dispatch(ctx, q, todo, docs)
}

// gradeWholePool grades every pooled doc, graded before or not, and has no
// per-doc fallback: a doc graded alone is normalised against itself. When the
// strategy fails, the query's docs stay unjudged.
func (r *Runner) gradeWholePool(ctx context.Context, ps PoolStrategy, q GradingQuery, pooled []pool.PooledDoc) (map[uuid.UUID]int, error) {
	docs, err := r.fetchDocs(ctx, pooled)
	if err != nil {
		return nil, fmt.Errorf("fetch docs: %w", err)
	}
	gradables := make([]GradingDoc, 0, len(pooled))
	for _, pd := range pooled {
		gd, ok := docs[pd.DocID]
		if !ok {
			slog.Warn("article not found during enrichment; leaving unjudged",
				"query", q.ID, "doc_id", pd.DocID)
			continue
		}
		gradables = append(gradables, gd)
	}

	results, err := ps.GradePool(ctx, q, gradables)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	var width *VectorWidthError
	var noTerms *NoQueryTermsError
	if errors.As(err, &width) || errors.As(err, &noTerms) {
		return nil, err
	}
	gradedByID := make(map[uuid.UUID]int, len(results))
	if err != nil {
		slog.Warn("pool grading failed; leaving the query unjudged", "query", q.ID, "error", err)
		return gradedByID, nil
	}
	for _, gd := range results {
		gradedByID[gd.DocID] = gd.Grade
	}
	return gradedByID, nil
}

// dispatch decides between batched and per-doc grading and runs it.
// Per-doc path uses bounded concurrency. Batched path uses sequential calls
// because each call is already a large LLM operation — parallelising buys
// little and increases the rate-limit risk.
func (r *Runner) dispatch(ctx context.Context, q GradingQuery, todo []pool.PooledDoc, docs map[uuid.UUID]GradingDoc) (map[uuid.UUID]int, error) {
	gradedByID := make(map[uuid.UUID]int, len(todo))

	gradables := make([]GradingDoc, 0, len(todo))
	for _, pd := range todo {
		gd, ok := docs[pd.DocID]
		if !ok {
			slog.Warn("article not found during enrichment; leaving unjudged",
				"query", q.ID, "doc_id", pd.DocID)
			continue
		}
		gradables = append(gradables, gd)
	}

	if bs, ok := r.cfg.Strategy.(BatchStrategy); ok {
		if err := r.runBatched(ctx, bs, q, gradables, gradedByID); err != nil {
			return nil, err
		}
	} else if err := r.runPerDoc(ctx, r.cfg.Strategy.(DocStrategy), q, gradables, gradedByID); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return gradedByID, nil
}

func (r *Runner) runBatched(ctx context.Context, bs BatchStrategy, q GradingQuery, gradables []GradingDoc, into map[uuid.UUID]int) error {
	size := r.cfg.BatchSize
	if size <= 0 {
		size = bs.PreferredBatchSize()
	}
	if size <= 0 {
		size = defaultBatchSize
	}

	batches := chunk(gradables, size)
	for i, batch := range batches {
		if err := ctx.Err(); err != nil {
			return err
		}
		histo := map[int]int{}
		results, err := bs.GradeBatch(ctx, q, batch)

		// A width mismatch is a property of the models, not of this batch, so
		// every remaining document would fail the same way. Retrying per-doc
		// would bury it and still write a complete annotations file.
		var width *VectorWidthError
		if errors.As(err, &width) {
			return err
		}

		// Partial batch — keep what came back, retry the missing IDs per-doc.
		var partial *PartialBatchError
		if errors.As(err, &partial) {
			slog.Warn("batch partial; retrying missing docs per-doc",
				"query", q.ID, "batch", i+1, "missing", len(partial.Missing))
			missingDocs := pickByID(batch, partial.Missing)
			perDoc := map[uuid.UUID]int{}
			if err := r.runPerDoc(ctx, bs, q, missingDocs, perDoc); err != nil {
				return err
			}
			for id, g := range perDoc {
				results = append(results, GradedDoc{DocID: id, Grade: g})
			}
		} else if err != nil {
			slog.Warn("batch failed; falling back to per-doc",
				"query", q.ID, "batch", i+1, "error", err)
			perDoc := map[uuid.UUID]int{}
			if err := r.runPerDoc(ctx, bs, q, batch, perDoc); err != nil {
				return err
			}
			results = nil
			for id, g := range perDoc {
				results = append(results, GradedDoc{DocID: id, Grade: g})
			}
		}

		for _, gd := range results {
			into[gd.DocID] = gd.Grade
			histo[gd.Grade]++
		}
		if r.cfg.OnBatch != nil {
			missing := len(batch) - len(results)
			r.cfg.OnBatch(BatchProgress{
				QueryID:   q.ID,
				BatchIdx:  i + 1,
				BatchN:    len(batches),
				Graded:    len(results),
				Missing:   missing,
				Histogram: histo,
			})
		}
	}
	return nil
}

// runPerDoc leaves a doc unjudged when its grade fails, except for a
// NoQueryTermsError, which fails every doc of the query and is returned.
func (r *Runner) runPerDoc(ctx context.Context, s DocStrategy, q GradingQuery, docs []GradingDoc, into map[uuid.UUID]int) error {
	sem := make(chan struct{}, r.cfg.Concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var queryErr error

	for _, d := range docs {
		wg.Add(1)
		go func(gd GradingDoc) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			grade, err := s.Grade(ctx, q, gd)
			var noTerms *NoQueryTermsError
			if errors.As(err, &noTerms) {
				mu.Lock()
				queryErr = err
				mu.Unlock()
				return
			}
			if err != nil {
				slog.Warn("grade failed; leaving unjudged",
					"query", q.ID, "doc_id", gd.ID, "error", err)
				return
			}
			mu.Lock()
			into[gd.ID] = grade
			mu.Unlock()
		}(d)
	}
	wg.Wait()
	return queryErr
}

func (r *Runner) fetchDocs(ctx context.Context, pooled []pool.PooledDoc) (map[uuid.UUID]GradingDoc, error) {
	if r.cfg.Reader == nil {
		return nil, fmt.Errorf("runner: storage reader is nil")
	}
	ids := make([]uuid.UUID, len(pooled))
	for i, pd := range pooled {
		ids[i] = pd.DocID
	}
	articles, err := r.cfg.Reader.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]GradingDoc, len(articles))
	for _, a := range articles {
		out[a.ID] = toGradingDoc(a)
	}
	return out, nil
}

func toGradingDoc(a document.Article) GradingDoc {
	return GradingDoc{
		ID:              a.ID,
		Title:           a.Title,
		Description:     a.Description,
		Content:         a.Content,
		PublishedAt:     a.PublishedAt,
		PublisherDomain: a.PublisherDomain,
	}
}

func chunk[T any](xs []T, size int) [][]T {
	if size <= 0 || len(xs) == 0 {
		return nil
	}
	out := make([][]T, 0, (len(xs)+size-1)/size)
	for i := 0; i < len(xs); i += size {
		end := i + size
		if end > len(xs) {
			end = len(xs)
		}
		out = append(out, xs[i:end])
	}
	return out
}

func pickByID(docs []GradingDoc, ids []uuid.UUID) []GradingDoc {
	want := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	out := make([]GradingDoc, 0, len(ids))
	for _, d := range docs {
		if _, ok := want[d.ID]; ok {
			out = append(out, d)
		}
	}
	return out
}

func indexExisting(jf *File) map[string]map[uuid.UUID]int {
	if jf == nil {
		return map[string]map[uuid.UUID]int{}
	}
	out := make(map[string]map[uuid.UUID]int, len(jf.Queries))
	for _, qe := range jf.Queries {
		m := make(map[uuid.UUID]int, len(qe.Docs))
		for _, d := range qe.Docs {
			m[d.DocID] = d.Grade
		}
		out[qe.QueryID] = m
	}
	return out
}
