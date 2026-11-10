package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/DjordjeVuckovic/tusker/internal/bench/dialect"
	"github.com/DjordjeVuckovic/tusker/internal/bench/engine"
	"github.com/DjordjeVuckovic/tusker/internal/bench/metrics"
	"github.com/DjordjeVuckovic/tusker/internal/bench/spec"
	"github.com/DjordjeVuckovic/tusker/internal/bench/suite"
	"github.com/DjordjeVuckovic/tusker/internal/storage"
	"github.com/google/uuid"
)

type Runner struct {
	config Config
}

func New(cfg Config) *Runner {
	return &Runner{config: cfg}
}

func (r *Runner) RunAll(
	ctx context.Context,
	bs *spec.BenchSpec,
	executors map[string]engine.Executor,
) (*BenchmarkResult, error) {
	if err := r.config.validate(); err != nil {
		return nil, err
	}
	if err := VerifyEmbeddingModel(bs.Engines, r.config.VectorStore); err != nil {
		return nil, err
	}

	br := &BenchmarkResult{Config: r.config}

	suites, err := LoadSuites(bs)
	if err != nil {
		return nil, err
	}
	if err := RegisterSearchTemplates(ctx, TemplateRegistration{Spec: bs, Suites: suites, Executors: executors}); err != nil {
		return nil, err
	}
	dialects, err := Dialects(bs)
	if err != nil {
		return nil, err
	}
	bindings := queryBindings(bs)

	for _, job := range bs.Jobs {
		jr, err := r.RunJob(ctx, JobRequest{
			Track:        bs.ID,
			Job:          job,
			Suite:        suites[job.Suite],
			Executors:    executors,
			Bindings:     bindings,
			Dialects:     dialects,
			QueryTimeout: bs.Runs.QueryTimeout,
		})
		if err != nil {
			return nil, fmt.Errorf("run job %q: %w", job.Name, err)
		}
		br.Jobs = append(br.Jobs, jr)
	}

	return br, nil
}

// VerifyEmbeddingModel rejects an engine that declares a different embedding
// model from the one the store embeds queries with. A vector template filters
// document vectors by the declared model while the query is embedded by the
// store, so a disagreement means every cosine distance is computed across two
// vector spaces — arithmetic that succeeds and ranks nothing. Engines that
// declare no model, and tracks with no store at all, are left alone.
func VerifyEmbeddingModel(engines map[string]spec.Engine, store storage.VectorStore) error {
	if store == nil {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(engines)) {
		declared, _ := engines[name].Params[suite.EmbeddingModelParam].(string)
		if declared == "" || declared == store.Model() {
			continue
		}
		return fmt.Errorf(
			"engine %q declares %s %q but queries are embedded with %q: set EMBEDDING_MODEL to the declared model without its @<dim> and EMBEDDING_DIM to that width, or fix the spec",
			name, suite.EmbeddingModelParam, declared, store.Model())
	}
	return nil
}

// queryBindings resolves every engine's query block and declared params once,
// so the per-query fan-out never reaches back into the spec.
func queryBindings(bs *spec.BenchSpec) map[string]spec.QueryBinding {
	bindings := make(map[string]spec.QueryBinding, len(bs.Engines))
	for name := range bs.Engines {
		bindings[name] = bs.QueryBinding(name)
	}
	return bindings
}

// Dialects maps each declared engine to the syntax its type reads params in.
func Dialects(bs *spec.BenchSpec) (map[string]dialect.Dialect, error) {
	dialects := make(map[string]dialect.Dialect, len(bs.Engines))
	for name, eng := range bs.Engines {
		d, err := dialect.For(eng.Type)
		if err != nil {
			return nil, fmt.Errorf("engine %q: %w", name, err)
		}
		dialects[name] = d
	}
	return dialects, nil
}

// LoadSuites loads every job's suite and checks its blocks so a broken one fails before any query runs.
func LoadSuites(bs *spec.BenchSpec) (map[string]*suite.LoadedSuite, error) {
	dialects, err := Dialects(bs)
	if err != nil {
		return nil, err
	}
	suites := map[string]*suite.LoadedSuite{}
	templateReaders := map[templateInSuite]engineDialect{}
	for _, job := range bs.Jobs {
		loaded, ok := suites[job.Suite]
		if !ok {
			ls, err := suite.LoadFromFile(job.Suite)
			if err != nil {
				return nil, fmt.Errorf("load suite for job %q: %w", job.Name, err)
			}
			suites[job.Suite] = ls
			loaded = ls
		}
		for _, engName := range job.Engines {
			binding := bs.QueryBinding(engName)
			if err := loaded.CheckArgsSupplied(binding.QuerySource, binding.Params); err != nil {
				return nil, fmt.Errorf("job %q engine %q: %w", job.Name, engName, err)
			}
			reader := engineDialect{engine: engName, dialect: dialects[engName]}
			if reader.dialect == nil {
				return nil, fmt.Errorf("job %q engine %q is not declared", job.Name, engName)
			}
			if err := checkEveryQueryHasABlock(loaded, boundEngine{name: engName, binding: binding}); err != nil {
				return nil, fmt.Errorf("job %q: %w", job.Name, err)
			}
			blocks, err := loaded.Blocks(binding.QuerySource, binding.Params)
			if err != nil {
				return nil, fmt.Errorf("job %q engine %q: %w", job.Name, engName, err)
			}
			for _, block := range blocks {
				if err := reader.dialect.Check(block); err != nil {
					return nil, fmt.Errorf("job %q engine %q query %q: %w", job.Name, engName, block.QueryID, err)
				}
				if block.Template == "" {
					continue
				}
				key := templateInSuite{suite: job.Suite, template: block.Template}
				if prev, seen := templateReaders[key]; seen && prev.dialect.Name() != reader.dialect.Name() {
					return nil, fmt.Errorf("template %q is read by engine %q as %s and by engine %q as %s",
						block.Template, prev.engine, prev.dialect.Name(), engName, reader.dialect.Name())
				}
				templateReaders[key] = reader
			}
		}
	}
	return suites, nil
}

// checkEveryQueryHasABlock rejects an engine whose block for one of the suite's
// queries is absent or empty. RunJob resolves every block without checking.
func checkEveryQueryHasABlock(ls *suite.LoadedSuite, eng boundEngine) error {
	source := eng.binding.QuerySource
	for _, q := range ls.Suite.Queries {
		if block, ok := q.Engines[source]; ok && hasStatement(block) {
			continue
		}
		if source != eng.name {
			return fmt.Errorf("query %q has no %q block for engine %q", q.ID, source, eng.name)
		}
		return fmt.Errorf("query %q has no block for engine %q", q.ID, eng.name)
	}
	return nil
}

// boundEngine is a job's engine and the binding it reads its queries through.
type boundEngine struct {
	name    string
	binding spec.QueryBinding
}

func hasStatement(block suite.EngineQuery) bool {
	return block.Query != "" || block.File != "" || block.Template != ""
}

type templateInSuite struct {
	suite    string
	template string
}

type engineDialect struct {
	engine  string
	dialect dialect.Dialect
}

// TemplateRegistration is a loaded track whose search templates are to be
// stored on the engines that render them.
type TemplateRegistration struct {
	Spec      *spec.BenchSpec
	Suites    map[string]*suite.LoadedSuite
	Executors map[string]engine.Executor
}

type templateOnEngine struct {
	engine   string
	template engine.SearchTemplate
}

// RegisterSearchTemplates stores templates on engines that render them, failing before storing any on a conflict.
func RegisterSearchTemplates(ctx context.Context, reg TemplateRegistration) error {
	dialects, err := Dialects(reg.Spec)
	if err != nil {
		return err
	}
	var pending []templateOnEngine
	sourceByID := map[string]string{}
	queued := map[templateOnEngine]bool{}
	for _, job := range reg.Spec.Jobs {
		ls := reg.Suites[job.Suite]
		for _, engName := range job.Engines {
			if d := dialects[engName]; d == nil || !d.StoresTemplates() {
				continue
			}
			exec, ok := reg.Executors[engName]
			if !ok {
				continue
			}
			if _, stores := exec.(engine.SearchTemplateRegistrar); !stores {
				return fmt.Errorf("engine %q reads %s templates but its executor cannot store them", engName, dialects[engName].Name())
			}
			source := reg.Spec.QueryBinding(engName).QuerySource
			for _, q := range ls.Suite.Queries {
				block, ok := q.Engines[source]
				if !ok || block.Template == "" {
					continue
				}
				tmpl, ok := ls.Registry.Get(block.Template)
				if !ok {
					return fmt.Errorf("query %q engine %q: template %q not found", q.ID, engName, block.Template)
				}
				stored := templateOnEngine{
					engine:   engName,
					template: engine.SearchTemplate{ID: engine.SearchTemplateID(reg.Spec.ID, tmpl.ID), Source: tmpl.Query},
				}
				if prev, seen := sourceByID[stored.template.ID]; seen && prev != tmpl.Query {
					return fmt.Errorf("two different templates would be stored as %q: rename one", stored.template.ID)
				}
				sourceByID[stored.template.ID] = tmpl.Query
				if !queued[stored] {
					queued[stored] = true
					pending = append(pending, stored)
				}
			}
		}
	}
	for _, p := range pending {
		if err := p.register(ctx, reg.Executors[p.engine], reg.Spec.Runs.QueryTimeout); err != nil {
			return fmt.Errorf("engine %q: %w", p.engine, err)
		}
	}
	return nil
}

func (p templateOnEngine) register(ctx context.Context, exec engine.Executor, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeoutOrDefault(timeout))
	defer cancel()
	return exec.(engine.SearchTemplateRegistrar).RegisterSearchTemplate(ctx, p.template)
}

// JobRequest is everything one job needs to run: its declaration, the loaded
// suite, the executors to drive, and how each engine reaches its queries. The
// suite must come from LoadSuites, which guarantees every engine a block for
// every query.
type JobRequest struct {
	// Track is the spec id, which namespaces the job's stored search templates.
	Track     string
	Job       spec.Job
	Suite     *suite.LoadedSuite
	Executors map[string]engine.Executor
	Bindings  map[string]spec.QueryBinding
	Dialects  map[string]dialect.Dialect
	// QueryTimeout bounds each execution; zero means spec.DefaultQueryTimeout.
	QueryTimeout time.Duration
}

func (r *Runner) RunJob(ctx context.Context, req JobRequest) (*JobResult, error) {
	jobExecutors := make(map[string]engine.Executor)
	for _, engName := range req.Job.Engines {
		exec, ok := req.Executors[engName]
		if !ok {
			return nil, fmt.Errorf("executor %q not found", engName)
		}
		jobExecutors[engName] = exec
	}

	jr := &JobResult{
		JobName:     req.Job.Name,
		Results:     make(map[string]map[string]QueryResult),
		EngineNames: req.Job.Engines,
	}

	jobReq := req
	jobReq.Executors = jobExecutors
	r.runQueries(ctx, jr, jobReq)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return jr, nil
}

func (r *Runner) runQueries(ctx context.Context, jr *JobResult, req JobRequest) {
	queries := req.Suite.Suite.Queries
	// Pre-populate order and result maps sequentially before launching any
	// goroutines. Goroutines only READ the outer jr.Results map (to get their
	// inner map pointer) and write only to their own inner map — no races.
	for i := range queries {
		jr.QueryOrder = append(jr.QueryOrder, queries[i].ID)
		jr.Results[queries[i].ID] = make(map[string]QueryResult)
	}

	// querySem controls how many queries execute concurrently.
	// QueryParallelismSerial (1) = one at a time → clean latency numbers.
	// QueryParallelismUnlimited (0) → all queries in parallel → faster pool/validate.
	qp := r.config.QueryParallelism
	if qp <= 0 {
		qp = len(queries)
	}
	querySem := make(chan struct{}, qp)

	// engineSem is shared across all query goroutines — it bounds total
	// concurrent engine calls globally, not just per query.
	ep := r.config.EngineParallelism
	if ep <= 0 {
		ep = len(jr.EngineNames)
	}
	engineSem := make(chan struct{}, ep)

	var wg sync.WaitGroup
	for i := range queries {
		q := &queries[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			querySem <- struct{}{}
			defer func() { <-querySem }()
			if ctx.Err() != nil {
				return
			}
			r.runEnginesForQuery(ctx, jr, q, req, engineSem)
		}()
	}
	wg.Wait()
}

// runEnginesForQuery fans out to all engines for a single query concurrently.
// Each goroutine writes only to its own index in the slots slice (no mutex),
// and the merge into jr.Results happens after all goroutines finish.
func (r *Runner) runEnginesForQuery(ctx context.Context, jr *JobResult, q *suite.Query, req JobRequest, engineSem chan struct{}) {
	judgments := r.judgmentsFor(q)
	queryVector := r.queryVector(ctx, req.Suite, q)

	type slot struct {
		engName string
		qr      QueryResult
		present bool
	}
	slots := make([]slot, len(jr.EngineNames))
	for idx, name := range jr.EngineNames {
		slots[idx].engName = name
	}

	var wg sync.WaitGroup
	for idx, engName := range jr.EngineNames {
		exec, ok := req.Executors[engName]
		if !ok {
			continue
		}
		idx, engName := idx, engName
		wg.Add(1)
		go func() {
			defer wg.Done()
			engineSem <- struct{}{}
			defer func() { <-engineSem }()

			// An engine with no declared binding reads its own block; an empty
			// QuerySource would instead match nothing and drop the query silently.
			binding, declared := req.Bindings[engName]
			if !declared {
				binding = spec.QueryBinding{QuerySource: engName}
			}
			resolved, err := q.ResolveEngineQuery(suite.ResolveOptions{
				Engine:      binding.QuerySource,
				Registry:    req.Suite.Registry,
				SuiteDir:    req.Suite.Dir,
				Defaults:    binding.Params,
				QueryVector: queryVector,
			})
			if err != nil {
				slots[idx] = slot{
					engName: engName,
					qr:      QueryResult{QueryID: q.ID, EngineName: engName, Error: fmt.Errorf("resolve query: %w", err)},
					present: true,
				}
				slog.Warn("resolve query failed", "query", q.ID, "engine", engName, "error", err)
				return
			}
			request := req.Dialects[engName].Request(req.Track, resolved)
			digest, err := requestDigest(request)
			if err != nil {
				slots[idx] = slot{
					engName: engName,
					qr:      QueryResult{QueryID: q.ID, EngineName: engName, Error: err},
					present: true,
				}
				slog.Warn("request digest failed", "query", q.ID, "engine", engName, "error", err)
				return
			}

			result := r.measure(ctx, measurement{
				queryID:  q.ID,
				engine:   engName,
				executor: exec,
				request:  request,
				timeout:  req.QueryTimeout,
			})

			var scores metrics.ScoreSet
			if result.err == nil && len(judgments) > 0 {
				scores = metrics.ComputeAll(result.rankedIDs, judgments, r.config.KValues, r.config.RelevanceThreshold)
			}
			if result.err != nil {
				slog.Warn("query failed", "query", q.ID, "engine", engName, "error", result.err)
			}

			slots[idx] = slot{
				engName: engName,
				qr: QueryResult{
					QueryID:       q.ID,
					Category:      q.Category,
					EngineName:    engName,
					RequestDigest: digest,
					Scores:        scores,
					RankedDocIDs:  result.rankedIDs,
					ReturnedCount: result.returnedCount,
					CorpusMatches: result.corpusMatches,
					Latency:       result.latencyStats,
					Error:         result.err,
				},
				present: true,
			}
		}()
	}
	wg.Wait()

	for _, s := range slots {
		if s.present {
			jr.Results[q.ID][s.engName] = s.qr
		}
	}
}

// queryVector embeds the query once when any of its blocks takes the query
// vector. Without a store, or when embedding fails, it returns nil and those
// blocks fail to resolve, recorded per engine.
func (r *Runner) queryVector(ctx context.Context, ls *suite.LoadedSuite, q *suite.Query) []float32 {
	if r.config.VectorStore == nil || !ls.NeedsQueryVector(q) {
		return nil
	}
	vec, err := r.config.VectorStore.QueryVector(ctx, q.Description)
	if err != nil {
		slog.Warn("query embedding failed; vector queries for this query will not resolve",
			"query", q.ID, "error", err)
		return nil
	}
	return vec
}

// judgmentsFor returns the relevance grades for a query. Priority: the
// runner-level Config.Judgments map (loaded by the CLI from the resolved
// annotations file) takes precedence over any judgments embedded in the suite
// (which is the case only when a suite is hand-edited — rare in v1).
func (r *Runner) judgmentsFor(q *suite.Query) map[uuid.UUID]int {
	if r.config.Judgments != nil {
		if perQuery, ok := r.config.Judgments[q.ID]; ok {
			out := make(map[uuid.UUID]int, len(perQuery))
			for idStr, grade := range perQuery {
				if id, err := uuid.Parse(idStr); err == nil {
					out[id] = grade
				}
			}
			return out
		}
	}
	return q.JudgmentMap()
}

type execResult struct {
	rankedIDs     []uuid.UUID
	returnedCount int
	corpusMatches *int64
	latencyStats  LatencyStats
	err           error
}

// measurement is one query on one engine, executed for warmup and then for the
// measured iterations.
type measurement struct {
	queryID  string
	engine   string
	executor engine.Executor
	request  engine.Request
	timeout  time.Duration
}

// measure fails on the first failed measured iteration: keeping the others
// would drop a timeout from the latency tail and report the query as answered.
func (r *Runner) measure(ctx context.Context, m measurement) execResult {
	for i := 0; i < r.config.WarmupRuns; i++ {
		if _, err := m.execute(ctx); err != nil {
			slog.Warn("warmup failed", "query", m.queryID, "engine", m.engine, "iteration", i+1, "error", err)
		}
	}

	var latencies []time.Duration
	var lastExec *engine.Execution
	for i := 0; i < r.config.Runs; i++ {
		result, err := m.execute(ctx)
		if err != nil {
			return execResult{err: fmt.Errorf("measured iteration %d of %d: %w", i+1, r.config.Runs, err)}
		}
		lastExec = result
		latencies = append(latencies, result.Latency)
	}

	if lastExec == nil {
		return execResult{err: errors.New("no measured iteration ran")}
	}
	ranked := lastExec.RankedDocIDs
	if depth := r.config.RankingDepth; depth > 0 && len(ranked) > depth {
		ranked = ranked[:depth]
	}
	return execResult{
		rankedIDs:     ranked,
		returnedCount: len(lastExec.RankedDocIDs),
		corpusMatches: lastExec.CorpusMatches,
		latencyStats:  ComputeLatencyStats(latencies),
	}
}

// execute runs once under the query deadline, which the runner owns so that no
// engine type is cut off sooner or later than another.
func (m measurement) execute(ctx context.Context) (*engine.Execution, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOrDefault(m.timeout))
	defer cancel()
	return m.executor.Execute(ctx, m.request)
}

func timeoutOrDefault(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return spec.DefaultQueryTimeout
	}
	return timeout
}
