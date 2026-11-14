package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/tadjov/tusker/internal/bench/judgment"
	"github.com/tadjov/tusker/internal/bench/meta"
	"github.com/tadjov/tusker/internal/bench/pool"
	"github.com/tadjov/tusker/internal/bench/spec"
	"github.com/tadjov/tusker/internal/bench/trackctx"
	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/factory"
	"github.com/tadjov/tusker/internal/storage/pg"
)

type judgeFlags struct {
	trackArg       string
	poolPath       string
	output         string
	strategy       string
	pg             string
	concurrency    int
	batchSize      int
	batchSeed      uint64
	resume         bool
	allowUnjudged  bool
	force          bool
	apiKey         string
	provider       string
	model          string
	effort         string
	apiBaseURL     string
	cliBinary      string
	embeddingBase  string
	embeddingModel string
}

// articleReaderOpener opens the store bench judge reads pooled articles from.
type articleReaderOpener func(cmd *cobra.Command, pgConn string) (storage.Reader, error)

type judgeCommand struct {
	flags      judgeFlags
	openReader articleReaderOpener
}

func newJudgeCmd() *cobra.Command { return newJudgeCmdWithReader(openArticleReader) }

func newJudgeCmdWithReader(openReader articleReaderOpener) *cobra.Command {
	var f judgeFlags
	cmd := &cobra.Command{
		Use:   "judge [track]",
		Short: "Grade a pool file with the chosen strategy",
		Long: `Grades every (query, doc) pair in the track's pool using one of:

  keyword     — deterministic token-overlap baseline (no network, no LLM)
  bm25        — pool-local Okapi BM25 (no network; rewards rare terms)
  vector      — the pool's ten docs nearest the query by cosine grade 3, the
                rest 0; doc vectors from PG (--pg), query vectors from
                trec/query-vectors, else Ollama (EMBEDDING_BASE_URL)
  hybrid      — BM25 + vector fusion, vectors as for vector
  llm         — LLM-as-judge, one doc per request (--batch N groups N);
                pick the endpoint with --provider
  manual      — writes grade:-1 placeholders for hand grading; re-running keeps
                existing hand grades and adds placeholders for new pool docs
                (--force starts over)

The llm strategy is vendor-neutral: --provider names the endpoint
(` + strings.Join(judgment.KnownProviders(), ", ") + `), and registering a new
provider is all it takes to add Codex or ChatGPT.

--model takes a full model id or a shorthand for the current generation
(` + strings.Join(judgment.ClaudeModelAliases(), ", ") + `) and defaults to ` + judgment.DefaultJudgeModel + `,
the model of the final cc-news judgments; --model haiku is the cheap option for
trial runs. --effort sets how much the model may think before it grades
(default low). The resolved id, the effort, the provider and the prompt version
are recorded in the artifact's judge block, so qrels always name the grader
that produced them.

Output goes to <track>/trec/annotations.<strategy>.yaml, where <strategy> is the
strategy for heuristic judges and the provider for llm judges.
Multiple strategies live side-by-side; switch which one bench run scores
against via --judgments <name>.

Resumable: re-run with the same --strategy and --resume to skip docs already
graded. bm25, vector and hybrid grade the whole pool at once, so they skip only
queries that are fully graded and regrade the rest whole. Atomic writes mean
Ctrl-C is safe.`,
		Example: `  bench judge tracks/global-news-dataset/fts_quality --strategy keyword
  bench judge tracks/global-news-dataset/fts_quality --strategy llm --provider claude-cli --model sonnet
  bench judge tracks/global-news-dataset/fts_quality --strategy llm --provider claude-api --model sonnet --batch 20 --resume
  bench judge --pool /tmp/p.yaml --strategy keyword --output /tmp/a.yaml`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return judgeCommand{flags: f, openReader: openReader}.execute(cmd, args)
		},
	}
	cmd.Flags().StringVar(&f.trackArg, "track", "", "Track path (relative to --track-root)")
	cmd.Flags().StringVar(&f.poolPath, "pool", "", "Override pool YAML path")
	cmd.Flags().StringVar(&f.output, "output", "", "Override annotations output path")
	cmd.Flags().StringVar(&f.strategy, "strategy", string(judgment.StrategyKeyword), "Judge strategy")
	cmd.Flags().StringVar(&f.pg, "pg", "", "Postgres connection (or set PG_CONNECTION_STRING)")
	cmd.Flags().IntVar(&f.concurrency, "concurrency", 4, "Parallel Grade calls (per-doc strategies)")
	cmd.Flags().IntVar(&f.batchSize, "batch", 0, "LLM docs per request (default one); bm25, vector and hybrid refuse it")
	cmd.Flags().Uint64Var(&f.batchSeed, "seed", 1, "Seed that shuffles each query's pool into --batch batches")
	cmd.Flags().BoolVar(&f.resume, "resume", false, "Skip docs already graded in --output; bm25, vector and hybrid skip only fully graded queries")
	cmd.Flags().BoolVar(&f.allowUnjudged, "allow-unjudged", false, "Exit 0 even when some pooled docs end unjudged; a query with no usable terms still fails")
	cmd.Flags().BoolVar(&f.force, "force", false, "Manual only: discard existing hand grades and rewrite the placeholders")
	cmd.Flags().StringVar(&f.apiKey, "api-key", "", "Anthropic API key (or set ANTHROPIC_API_KEY)")
	cmd.Flags().StringVar(&f.provider, "provider", string(judgment.DefaultProvider),
		"LLM endpoint for --strategy llm: "+strings.Join(judgment.KnownProviders(), " | "))
	cmd.Flags().StringVar(&f.model, "model", judgment.DefaultJudgeModel,
		"LLM judge model id or alias ("+strings.Join(judgment.ClaudeModelAliases(), " | ")+"); haiku for cheap trial runs")
	cmd.Flags().StringVar(&f.effort, "effort", string(judgment.EffortLow),
		"Effort the llm judge's model grades at: "+strings.Join(judgment.KnownEfforts(), " | "))
	cmd.Flags().StringVar(&f.apiBaseURL, "api-base", "", "Anthropic API base URL")
	cmd.Flags().StringVar(&f.cliBinary, "cli-binary", "", "claude CLI binary path")
	cmd.Flags().StringVar(&f.embeddingBase, "embedding-base", "", "Embedding endpoint for vector/hybrid (or EMBEDDING_BASE_URL)")
	cmd.Flags().StringVar(&f.embeddingModel, "embedding-model", "", "Embedding model for vector/hybrid (or EMBEDDING_MODEL)")
	return cmd
}

func (j judgeCommand) execute(cmd *cobra.Command, args []string) error {
	in := trackInputs(j.flags.trackArg, args)
	in.PoolPath = j.flags.poolPath
	in.OutputPath = j.flags.output
	return forEachTrack(cmd.Context(), cmd.OutOrStdout(), in, func(tr *trackctx.Track) error {
		return j.judgeTrack(cmd, tr)
	})
}

func (j judgeCommand) judgeTrack(cmd *cobra.Command, tr *trackctx.Track) error {
	f := j.flags
	poolPath := f.poolPath
	if poolPath == "" {
		poolPath = tr.Pool
	}
	pf, err := pool.ReadPoolFile(poolPath)
	if err != nil {
		return fmt.Errorf("read pool: %w", err)
	}

	kind, provider, err := resolveJudge(f)
	if err != nil {
		return err
	}
	if f.force && kind != judgment.StrategyManual {
		return fmt.Errorf("--force applies only to --strategy manual; %s rewrites its file on every run without --resume", kind)
	}
	// Heuristic judges file under their strategy; llm judges file under their
	// provider, so two vendors can grade the same track side by side.
	outPath := f.output
	if outPath == "" {
		outPath = tr.JudgmentsPath(judgment.JudgmentSetName(kind, provider))
	}

	// Manual grades nothing, so it needs no article reader.
	if kind == judgment.StrategyManual {
		var existing *judgment.File
		if !f.force {
			if existing, err = judgment.ReadFileIfExists(outPath); err != nil {
				return fmt.Errorf("read existing manual judgments: %w", err)
			}
			if existing != nil && existing.Judge().Strategy != string(judgment.StrategyManual) {
				return fmt.Errorf("%s was graded by %q, not by hand: merging would relabel its grades as manual; pass --force to replace it",
					outPath, existing.Judge())
			}
		}
		jf, err := manualJudgments{pool: pf, poolPath: poolPath, path: outPath}.write(existing)
		if err != nil {
			return err
		}
		printDone(cmd.OutOrStdout(), fmt.Sprintf("Manual template written: %s  (queries=%d)", outPath, len(jf.Queries)))
		return nil
	}

	opts := judgment.StrategyOptions{
		APIKey:      envOrFlag("ANTHROPIC_API_KEY", f.apiKey),
		Provider:    provider,
		Model:       f.model,
		APIBaseURL:  f.apiBaseURL,
		CLIBinary:   f.cliBinary,
		Concurrency: f.concurrency,
	}
	if kind == judgment.StrategyLLM {
		if opts.Effort, err = judgment.ParseEffort(f.effort); err != nil {
			return fmt.Errorf("--effort: %w", err)
		}
	}
	if kind == judgment.StrategyVector || kind == judgment.StrategyHybrid {
		store, model, closeStore, err := buildVectorStore(cmd.Context(), f, tr)
		if err != nil {
			return err
		}
		defer closeStore()
		opts.VectorStore = store
		opts.EmbeddingModel = model
	}

	strat, err := judgment.NewStrategy(kind, opts)
	if err != nil {
		return err
	}

	reader, err := j.openReader(cmd, f.pg)
	if err != nil {
		return err
	}

	cfg := judgment.RunnerConfig{
		Strategy:    strat,
		Reader:      reader,
		Concurrency: f.concurrency,
		BatchSize:   f.batchSize,
		BatchSeed:   f.batchSeed,
	}
	// Every flush carries this meta, so a file left by an interrupted run names
	// its run and grader.
	judge := cfg.Judge()
	runMeta := meta.New("judge")
	runMeta.Judge = &judge
	runMeta.PoolRef = poolPath
	runMeta.PoolRunID = pf.Meta.RunID
	runMeta.RelevanceScale = []int{0, 1, 2, 3}
	writer := judgment.NewIncrementalWriter(outPath, runMeta)
	if f.resume {
		cfg.Existing, err = writer.LoadPrior(pf)
		if err != nil {
			return fmt.Errorf("load prior judgments: %w", err)
		}
		if cfg.Existing != nil {
			if err := checkResumeCompat(cfg.Existing, judge); err != nil {
				return err
			}
			if err := checkPoolBinding(cfg.Existing.Meta.PoolRunID, pf); err != nil {
				return fmt.Errorf("--resume: %w\n  • re-run without --resume to grade the current pool from scratch", err)
			}
			printWarn(cmd.OutOrStdout(), fmt.Sprintf("Resume: loaded %d prior queries from %s", len(cfg.Existing.Queries), outPath))
		}
	}
	cfg.Sink = writer.Append
	cfg.OnQueryStart = func(qid string, total, skipped int) {
		if skipped > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "%s grading %d docs %s\n",
				cCyan.Sprintf("[%s]", qid), total-skipped,
				cDim.Sprintf("(%d already done)", skipped))
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "%s grading %d docs\n",
				cCyan.Sprintf("[%s]", qid), total)
		}
	}
	cfg.OnBatch = func(bp judgment.BatchProgress) {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s batch %d/%d: graded=%d missing=%d %s\n",
			cDim.Sprint("└"),
			bp.BatchIdx, bp.BatchN, bp.Graded, bp.Missing,
			cDim.Sprint(formatHistogram(bp.Histogram)))
	}
	cfg.OnQueryDone = func(qp judgment.QueryProgress) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s graded=%d skipped=%d unjudged=%d %s\n",
			cCyan.Sprintf("[%s]", qp.QueryID),
			cOK.Sprint("done:"),
			qp.Graded, qp.Skipped, qp.Unjudged,
			cDim.Sprint(formatHistogram(qp.Histogram)))
	}
	jrunner := judgment.NewRunner(cfg)

	final, runErr := jrunner.Run(cmd.Context(), pf)
	if runErr != nil && !judgment.FailsWholeQuery(runErr) {
		return fmt.Errorf("judge run: %w", runErr)
	}

	// Run returns only the queries still in the pool, so a query dropped since
	// the last run is not carried over.
	final.Meta = runMeta
	if err := judgment.WriteFile(final, outPath); err != nil {
		return fmt.Errorf("finalise judgments: %w", err)
	}

	printDone(cmd.OutOrStdout(), fmt.Sprintf("Judgments written: %s  (judge=%s  queries=%d  run_id=%s)",
		outPath, judge, len(final.Queries), final.Meta.RunID))
	if runErr != nil {
		return fmt.Errorf("judge run: %w", runErr)
	}
	return requireJudged(final, f.allowUnjudged)
}

// resolveJudge turns --strategy/--provider into the pair the strategy factory
// needs. --provider applies only to the llm strategy; naming a judgment set
// ("claude-cli") in --strategy is redirected to the explicit form rather than
// silently accepted, so there is one way to write a judge.
func resolveJudge(f judgeFlags) (judgment.StrategyKind, judgment.Provider, error) {
	kind := judgment.StrategyKind(f.strategy)
	switch kind {
	case judgment.StrategyLLM:
		return kind, judgment.Provider(f.provider), nil
	case judgment.StrategyKeyword, judgment.StrategyBM25, judgment.StrategyVector,
		judgment.StrategyHybrid, judgment.StrategyManual:
		return kind, "", nil
	}

	// A provider name in --strategy is the old spelling. Redirect rather than
	// accept it, so there is exactly one way to write a judge.
	if _, p, err := judgment.ParseSelector(f.strategy); err == nil {
		return "", "", fmt.Errorf("%q names a provider, not a strategy — use --strategy llm --provider %s",
			f.strategy, p)
	}
	return "", "", fmt.Errorf("unknown strategy %q (known: %s)",
		f.strategy, strings.Join(judgment.KnownJudgeStrategies(), ", "))
}

// checkResumeCompat verifies that a prior judgments file is safe to resume
// under the judge block now. A difference in any field of the judge block means
// the two runs disagree on who is grading, and appending would corrupt the set.
func checkResumeCompat(prior *judgment.File, now meta.Judge) error {
	was := prior.Judge()
	if (was == meta.Judge{}) {
		return nil
	}
	differences := was.Differences(now)
	if len(differences) == 0 {
		return nil
	}
	return fmt.Errorf(
		"--resume judge mismatch: the existing file was graded by another judge (%s)\n"+
			"  • re-run without --resume to re-grade cleanly under the new judge",
		strings.Join(differences, "; "))
}

// buildVectorStore builds the vector/hybrid judges' store: query vectors from
// the track's query-vectors package when it has one, from Ollama otherwise, and
// document vectors from Postgres.
func buildVectorStore(ctx context.Context, f judgeFlags, tr *trackctx.Track) (storage.VectorStore, string, func(), error) {
	noop := func() {}
	pgConn := envOrFlag("PG_CONNECTION_STRING", f.pg)
	if pgConn == "" {
		return nil, "", noop, fmt.Errorf("vector/hybrid judging requires --pg or PG_CONNECTION_STRING")
	}
	packageDir, found, err := queryVectorsPackage(tr)
	if err != nil {
		return nil, "", noop, err
	}
	if found {
		bs, err := spec.LoadFromFile(tr.Spec)
		if err != nil {
			return nil, "", noop, fmt.Errorf("load spec to check query vectors: %w", err)
		}
		store, closeStore, err := openQueryVectors(ctx, queryVectorsSource{spec: bs, packageDir: packageDir, postgres: pgConn})
		if err != nil {
			return nil, "", noop, err
		}
		return store, store.Model(), closeStore, nil
	}
	baseURL := envOrFlag("EMBEDDING_BASE_URL", f.embeddingBase)
	if baseURL == "" {
		return nil, "", noop, fmt.Errorf("vector/hybrid judging requires trec/query-vectors, or --embedding-base or EMBEDDING_BASE_URL (ollama endpoint)")
	}
	client, err := embedding.NewOllamaClient(baseURL)
	if err != nil {
		return nil, "", noop, fmt.Errorf("embedding client: %w", err)
	}
	dim, err := embedding.DimFromEnv()
	if err != nil {
		return nil, "", noop, err
	}
	store, err := factory.NewVectorStore(ctx, factory.VectorStoreConfig{
		PgConnStr:       pgConn,
		EmbeddingClient: client,
		Model:           envOrFlag("EMBEDDING_MODEL", f.embeddingModel),
		Dim:             dim,
	})
	if err != nil {
		return nil, "", noop, err
	}
	return store, store.Model(), noop, nil
}

// openArticleReader creates a PG reader for article enrichment. Centralised so
// the no-key-needed case (manual strategy) can skip it cleanly.
func openArticleReader(cmd *cobra.Command, pgConn string) (storage.Reader, error) {
	conn := envOrFlag("PG_CONNECTION_STRING", pgConn)
	if conn == "" {
		return nil, fmt.Errorf("reading pooled articles requires --pg or PG_CONNECTION_STRING")
	}
	reader, err := factory.NewReader(cmd.Context(), factory.StorageConfig{
		Type: storage.PG,
		Pg:   &pg.PoolConfig{ConnStr: conn},
	})
	if err != nil {
		return nil, fmt.Errorf("create reader: %w", err)
	}
	return reader, nil
}

// buildManualJudgments writes a -1 placeholder for every pooled doc that has no
// hand grade yet. Everything in existing survives, including docs that have
// since left the pool, so re-running never erases a grade.
func buildManualJudgments(pf *pool.PoolFile, existing *judgment.File) *judgment.File {
	jf := &judgment.File{}
	if existing != nil {
		jf.Queries = existing.Queries
	}
	entryIndex := make(map[string]int, len(jf.Queries))
	for i, qe := range jf.Queries {
		entryIndex[qe.QueryID] = i
	}
	for _, entry := range pf.Queries {
		i, ok := entryIndex[entry.QueryID]
		if !ok {
			i = len(jf.Queries)
			entryIndex[entry.QueryID] = i
			jf.Queries = append(jf.Queries, judgment.Entry{QueryID: entry.QueryID})
		}
		known := make(map[uuid.UUID]bool, len(jf.Queries[i].Docs))
		for _, d := range jf.Queries[i].Docs {
			known[d.DocID] = true
		}
		for _, d := range entry.Docs {
			if !known[d.DocID] {
				jf.Queries[i].Docs = append(jf.Queries[i].Docs, judgment.GradedDoc{DocID: d.DocID, Grade: judgment.GradeUnjudged})
			}
		}
	}
	return jf
}

// manualJudgments is a hand-grading file bound to the pool it was built from.
type manualJudgments struct {
	pool     *pool.PoolFile
	poolPath string
	path     string
}

// write merges existing (nil starts over) with a -1 placeholder for every
// pooled doc it lacks, binds the result to the pool and writes it.
func (m manualJudgments) write(existing *judgment.File) (*judgment.File, error) {
	jf := buildManualJudgments(m.pool, existing)
	judge := judgment.NewManualStrategy().Describe()
	jf.Meta = meta.New("judge")
	jf.Meta.Judge = &judge
	jf.Meta.PoolRef = m.poolPath
	jf.Meta.PoolRunID = m.pool.Meta.RunID
	if err := judgment.WriteFile(jf, m.path); err != nil {
		return nil, fmt.Errorf("write manual judgments: %w", err)
	}
	return jf, nil
}

// requireJudged fails when a pooled doc has no grade: bench run would score it
// as non-relevant against whichever engine retrieved it.
func requireJudged(jf *judgment.File, allowUnjudged bool) error {
	_, unjudged := jf.GradeCounts()
	if unjudged == 0 || allowUnjudged {
		return nil
	}
	return fmt.Errorf("%d pooled docs are unjudged: re-run with --resume to retry them, or pass --allow-unjudged to keep the file as is", unjudged)
}

func formatHistogram(h map[int]int) string {
	if len(h) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("(")
	for g := 3; g >= 0; g-- {
		if g < 3 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%d:%d", g, h[g])
	}
	b.WriteString(")")
	return b.String()
}
