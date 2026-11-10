package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/DjordjeVuckovic/tusker/internal/bench/engine"
	"github.com/DjordjeVuckovic/tusker/internal/bench/meta"
	"github.com/DjordjeVuckovic/tusker/internal/bench/pool"
	"github.com/DjordjeVuckovic/tusker/internal/bench/runner"
	"github.com/DjordjeVuckovic/tusker/internal/bench/spec"
	"github.com/DjordjeVuckovic/tusker/internal/bench/suite"
	"github.com/DjordjeVuckovic/tusker/internal/bench/trackctx"
	"github.com/spf13/cobra"
)

type poolFlags struct {
	trackArg     string
	specPath     string
	output       string
	depth        int
	allowPartial bool
}

func newPoolCmd() *cobra.Command {
	var f poolFlags
	cmd := &cobra.Command{
		Use:   "pool [track]",
		Short: "Run queries through all engines, write a TREC-style pool",
		Long: `Generates a deduplicated pool of candidate docs per query, ready to be
judged. Output goes to <track>/trec/pool.yaml by default; override
with --output for ad-hoc files.

The pool file carries a meta block (run_id, tool, engines, depth) so later
artifacts can attest which pool they were derived from.

Any (query, engine) execution that errors fails the pool, since its documents
would be missing from it. --allow-partial writes the pool anyway, lists only
the engines that answered in meta.engines and records the failed pairs in
meta.dropped_executions.`,
		Example: `  bench pool tracks/global-news-dataset/fts_quality
  bench pool tracks/global-news-dataset/fts_quality --depth 50
  bench pool --track tracks/global-news-dataset/fts_quality --output /tmp/adhoc-pool.yaml`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return executePool(cmd, f, args)
		},
	}
	cmd.Flags().StringVar(&f.trackArg, "track", "", "Track path (relative to --track-root)")
	cmd.Flags().StringVar(&f.specPath, "spec", "", "Override spec.yaml path")
	cmd.Flags().IntVar(&f.depth, "depth", 0, "Top-K per engine (0 = spec.defaults.pool_depth or 100)")
	cmd.Flags().StringVar(&f.output, "output", "", "Override pool output path")
	cmd.Flags().BoolVar(&f.allowPartial, "allow-partial", false,
		"Write the pool even when some (query, engine) executions failed, recording them in meta.dropped_executions")
	return cmd
}

func executePool(cmd *cobra.Command, f poolFlags, args []string) error {
	in := trackInputs(f.trackArg, args)
	in.SpecPath = f.specPath
	in.OutputPath = f.output
	return forEachTrack(cmd.Context(), cmd.OutOrStdout(), in, func(tr *trackctx.Track) error {
		return poolTrack(cmd, f, tr)
	})
}

func poolTrack(cmd *cobra.Command, f poolFlags, tr *trackctx.Track) error {
	bs, err := spec.LoadFromFile(tr.Spec)
	if err != nil {
		return fmt.Errorf("load spec: %w", err)
	}

	depth := f.depth
	if depth == 0 {
		depth = bs.Defaults.PoolDepth
	}
	if depth == 0 {
		depth = 100
	}

	runCfg := runner.Config{
		KValues:          []int{depth},
		Runs:             1,
		QueryParallelism: runner.QueryParallelismUnlimited,
	}

	printSpecWarnings(cmd.OutOrStdout(), bs)

	vectorStore, closeVectorStore, err := buildQueryVectorStore(cmd.Context(), bs, tr)
	if err != nil {
		return fmt.Errorf("build vector store: %w", err)
	}
	defer closeVectorStore()
	if err := requireEmbedder(bs, vectorStore); err != nil {
		return err
	}
	runCfg.VectorStore = vectorStore

	executors, cleanup, err := createExecutors(cmd.Context(), bs)
	if err != nil {
		return fmt.Errorf("create executors: %w", err)
	}
	defer cleanup()

	r := runner.New(runCfg)
	sp := startSpinner("Pooling " + tr.Name() + "…")
	result, err := r.RunAll(cmd.Context(), bs, executors)
	sp.Stop()
	if err != nil {
		return fmt.Errorf("pool run: %w", err)
	}
	dropped := droppedExecutions(result)
	if len(dropped) > 0 && !f.allowPartial {
		return incompletePoolError(dropped)
	}

	descs, cats, err := collectQueryDescriptions(bs)
	if err != nil {
		return fmt.Errorf("load query descriptions: %w", err)
	}

	pf := buildPoolFile(result, descs, cats, depth)
	pf.Meta = meta.New("pool")
	pf.Meta.SpecID = bs.ID
	pf.Meta.PoolDepth = depth
	pf.Meta.Engines = collectEngines(bs, result)
	pf.Meta.DroppedExecutions = dropped

	outPath := f.output
	if outPath == "" {
		outPath = tr.Pool
	}
	for _, se := range pool.ShallowEngines(pf, depth) {
		printWarn(cmd.OutOrStdout(), fmt.Sprintf(
			"engine %s contributed at most %d documents to any query; its pool is %d deep, not the %d stamped on meta.pool_depth",
			se.Name, se.MaxReturned, se.MaxReturned, depth))
	}

	if err := pool.WritePoolFile(pf, outPath); err != nil {
		return fmt.Errorf("write pool: %w", err)
	}
	printDone(cmd.OutOrStdout(), fmt.Sprintf("Pool written: %s  (queries=%d  run_id=%s)", outPath, len(pf.Queries), pf.Meta.RunID))
	return nil
}

// buildPoolFile merges results across all jobs into one entry per query_id.
// Multiple jobs commonly share a suite (e.g. a 3-engine job + an all-engines
// job), so the same query_id surfaces in several JobResults. We union the
// per-engine executions by query_id so each query is pooled — and later judged
// — exactly once, regardless of how many jobs touched it.
func buildPoolFile(result *runner.BenchmarkResult, descs, cats map[string]string, depth int) *pool.PoolFile {
	pf := &pool.PoolFile{}

	var order []string                                       // first-seen query order
	byQuery := make(map[string]map[string]*engine.Execution) // query_id → engine → execution
	digests := make(map[string]map[string]string)
	jobSeen := make(map[string]struct{})
	var jobNames []string

	for _, jr := range result.Jobs {
		if _, ok := jobSeen[jr.JobName]; !ok {
			jobSeen[jr.JobName] = struct{}{}
			jobNames = append(jobNames, jr.JobName)
		}
		for _, qID := range jr.QueryOrder {
			execs, ok := byQuery[qID]
			if !ok {
				execs = make(map[string]*engine.Execution)
				byQuery[qID] = execs
				digests[qID] = make(map[string]string)
				order = append(order, qID)
			}
			engResults := jr.Results[qID]
			for _, engName := range jr.EngineNames {
				// First job to contribute an engine wins; the same engine on
				// the same suite/query produces an identical ranked list, so
				// there's nothing to merge — we only need engines not yet seen.
				if _, exists := execs[engName]; exists {
					continue
				}
				qr, ok := engResults[engName]
				if !ok || qr.Error != nil {
					continue
				}
				execs[engName] = &engine.Execution{
					RankedDocIDs:  qr.RankedDocIDs,
					CorpusMatches: qr.CorpusMatches,
				}
				digests[qID][engName] = qr.RequestDigest
			}
		}
	}

	pf.SuiteName = strings.Join(jobNames, ",")
	for _, qID := range order {
		pf.Queries = append(pf.Queries, pool.PoolEntry{
			QueryID:        qID,
			QueryDesc:      descs[qID],
			Category:       cats[qID],
			Docs:           pool.PoolResults(byQuery[qID], depth),
			RequestDigests: digests[qID],
		})
	}
	return pf
}

func collectQueryDescriptions(bs *spec.BenchSpec) (descs, cats map[string]string, err error) {
	descs = make(map[string]string)
	cats = make(map[string]string)
	seen := make(map[string]struct{})
	for _, job := range bs.Jobs {
		if _, ok := seen[job.Suite]; ok {
			continue
		}
		seen[job.Suite] = struct{}{}
		ls, err := suite.LoadFromFile(job.Suite)
		if err != nil {
			return nil, nil, fmt.Errorf("load suite %q for job %q: %w", job.Suite, job.Name, err)
		}
		for _, q := range ls.Suite.Queries {
			if q.Description != "" {
				descs[q.ID] = q.Description
			}
			if q.Category != "" {
				cats[q.ID] = q.Category
			}
		}
	}
	return descs, cats, nil
}

// collectEngines lists, in spec order, the engines that answered at least one
// query, so an engine whose every execution failed is not claimed by the pool.
func collectEngines(bs *spec.BenchSpec, result *runner.BenchmarkResult) []string {
	answered := make(map[string]bool)
	for cell := range answeredCells(result) {
		answered[cell.engine] = true
	}
	seen := make(map[string]struct{})
	var out []string
	for _, job := range bs.Jobs {
		for _, eng := range job.Engines {
			if _, ok := seen[eng]; ok || !answered[eng] {
				continue
			}
			seen[eng] = struct{}{}
			out = append(out, eng)
		}
	}
	return out
}

type poolCell struct {
	queryID string
	engine  string
}

func answeredCells(result *runner.BenchmarkResult) map[poolCell]bool {
	answered := make(map[poolCell]bool)
	for _, jr := range result.Jobs {
		for queryID, byEngine := range jr.Results {
			for engineName, qr := range byEngine {
				if qr.Error == nil {
					answered[poolCell{queryID: queryID, engine: engineName}] = true
				}
			}
		}
	}
	return answered
}

// droppedExecutions lists the (query, engine) pairs no job got an answer for,
// sorted by query then engine.
func droppedExecutions(result *runner.BenchmarkResult) []meta.DroppedExecution {
	answered := answeredCells(result)
	failures := make(map[poolCell]string)
	for _, jr := range result.Jobs {
		for queryID, byEngine := range jr.Results {
			for engineName, qr := range byEngine {
				cell := poolCell{queryID: queryID, engine: engineName}
				if qr.Error == nil || answered[cell] {
					continue
				}
				if _, recorded := failures[cell]; !recorded {
					failures[cell] = qr.Error.Error()
				}
			}
		}
	}
	dropped := make([]meta.DroppedExecution, 0, len(failures))
	for cell, msg := range failures {
		dropped = append(dropped, meta.DroppedExecution{QueryID: cell.queryID, Engine: cell.engine, Error: msg})
	}
	slices.SortFunc(dropped, func(a, b meta.DroppedExecution) int {
		return cmp.Or(cmp.Compare(a.QueryID, b.QueryID), cmp.Compare(a.Engine, b.Engine))
	})
	return dropped
}

func incompletePoolError(dropped []meta.DroppedExecution) error {
	cells := make([]string, 0, len(dropped))
	for _, d := range dropped {
		cells = append(cells, fmt.Sprintf("%s/%s (%s)", d.QueryID, d.Engine, d.Error))
	}
	return fmt.Errorf("%d (query, engine) executions failed, so the pool would miss their documents: %s\n"+
		"  • fix the engines and re-run, or pass --allow-partial to write the pool without them",
		len(dropped), strings.Join(cells, "; "))
}
