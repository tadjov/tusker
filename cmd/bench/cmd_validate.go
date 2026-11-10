package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/DjordjeVuckovic/tusker/internal/bench/dialect"
	"github.com/DjordjeVuckovic/tusker/internal/bench/engine"
	"github.com/DjordjeVuckovic/tusker/internal/bench/runner"
	"github.com/DjordjeVuckovic/tusker/internal/bench/spec"
	"github.com/DjordjeVuckovic/tusker/internal/bench/suite"
	"github.com/DjordjeVuckovic/tusker/internal/bench/trackctx"
	"github.com/DjordjeVuckovic/tusker/internal/storage"
	"github.com/spf13/cobra"
)

type validateFlags struct {
	trackArg string
	specPath string
	failFast bool
}

func newValidateCmd() *cobra.Command {
	var f validateFlags
	cmd := &cobra.Command{
		Use:   "validate [track]",
		Short: "Dry-run every query through each engine and report broken ones",
		Long: `Validates spec + suite ahead of a real pool/run:

  - every query is given the args its statement takes
  - postgres queries pass EXPLAIN with their args (syntax, columns, operators)
  - elasticsearch search templates are stored, rendered with their params, and
    the rendered body passes _validate/query (JSON, fields, types), as do
    inline Query DSL bodies
  - api descriptors parse as {method, path, body?, params?}

Returns non-zero exit if any query fails — wire it into CI.`,
		Example: `  bench validate tracks/global-news-dataset/fts_quality
  bench validate --track tracks/global-news-dataset/fts_quality --fail-fast`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return executeValidate(cmd, f, args)
		},
	}
	cmd.Flags().StringVar(&f.trackArg, "track", "", "Track path (relative to --track-root)")
	cmd.Flags().StringVar(&f.specPath, "spec", "", "Override spec.yaml path")
	cmd.Flags().BoolVar(&f.failFast, "fail-fast", false, "Stop at first failure")
	return cmd
}

type validateRow struct {
	queryID string
	engine  string
	status  string
	detail  string
}

func executeValidate(cmd *cobra.Command, f validateFlags, args []string) error {
	in := trackInputs(f.trackArg, args)
	in.SpecPath = f.specPath
	return forEachTrack(cmd.Context(), cmd.OutOrStdout(), in, func(tr *trackctx.Track) error {
		return validateTrack(cmd, f, tr)
	})
}

func validateTrack(cmd *cobra.Command, f validateFlags, tr *trackctx.Track) error {
	bs, err := spec.LoadFromFile(tr.Spec)
	if err != nil {
		return fmt.Errorf("load spec: %w", err)
	}
	printSpecWarnings(cmd.OutOrStdout(), bs)

	// A semantic/hybrid track without an embedder can never resolve its vector
	// queries; fail here rather than letting every row stub a fake vector and
	// report a misleading OK.
	vectorStore, closeVectorStore, err := buildQueryVectorStore(cmd.Context(), bs, tr)
	if err != nil {
		return fmt.Errorf("build vector store: %w", err)
	}
	defer closeVectorStore()
	if err := requireEmbedder(bs, vectorStore); err != nil {
		return err
	}
	// The declared model reaches the SQL as a literal; the store embeds the
	// query. A dry run that renders them apart would report OK on queries that
	// rank across two vector spaces.
	if err := runner.VerifyEmbeddingModel(bs.Engines, vectorStore); err != nil {
		return err
	}

	executors, cleanup, err := createExecutors(cmd.Context(), bs)
	if err != nil {
		return fmt.Errorf("create executors: %w", err)
	}
	defer cleanup()

	suites, err := runner.LoadSuites(bs)
	if err != nil {
		return err
	}
	dialects, err := runner.Dialects(bs)
	if err != nil {
		return err
	}
	if err := runner.RegisterSearchTemplates(cmd.Context(), runner.TemplateRegistration{
		Spec: bs, Suites: suites, Executors: executors,
	}); err != nil {
		return err
	}

	var rows []validateRow
	failures := 0
	// seen deduplicates (suitePath, queryID, engineName) triples — two jobs
	// that share the same suite and engines would otherwise re-validate the
	// same pairs, doubling traffic and output noise.
	seen := map[string]struct{}{}

	for _, job := range bs.Jobs {
		ls := suites[job.Suite]
		for _, q := range ls.Suite.Queries {
			for _, engName := range job.Engines {
				key := job.Suite + "\x00" + q.ID + "\x00" + engName
				if _, done := seen[key]; done {
					continue
				}
				seen[key] = struct{}{}

				row := validateOne(cmd.Context(), validateInput{
					query:        q,
					track:        bs.ID,
					engineName:   engName,
					binding:      bs.QueryBinding(engName),
					loaded:       ls,
					executor:     executors[engName],
					store:        vectorStore,
					dialect:      dialects[engName],
					queryTimeout: bs.Runs.QueryTimeout,
				})
				if err := cmd.Context().Err(); err != nil {
					return err
				}
				rows = append(rows, row)
				if row.status != "OK" {
					failures++
					if f.failFast {
						printValidateRows(cmd.OutOrStdout(), rows)
						return fmt.Errorf("validation failed (fail-fast)")
					}
				}
			}
		}
	}

	warnKindDrift(cmd.OutOrStdout(), bs, suites)

	printValidateRows(cmd.OutOrStdout(), rows)
	fmt.Fprintln(cmd.OutOrStdout())
	if failures > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Total: %d checks  %s\n",
			len(rows), cFail.Sprintf("%d failed", failures))
		return fmt.Errorf("%d query/engine pair(s) failed validation", failures)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Total: %d checks  %s\n",
		len(rows), cOK.Sprint("all passed"))
	return nil
}

// validateInput is one (query, engine) pair to dry-run, with everything needed
// to resolve and check it.
type validateInput struct {
	query        suite.Query
	track        string
	engineName   string
	binding      spec.QueryBinding
	loaded       *suite.LoadedSuite
	executor     engine.Executor
	store        storage.VectorStore
	dialect      dialect.Dialect
	queryTimeout time.Duration
}

func validateOne(ctx context.Context, in validateInput) validateRow {
	q, exec := in.query, in.executor
	row := validateRow{queryID: q.ID, engine: in.engineName}

	var queryVector []float32
	if in.loaded.NeedsQueryVector(&q) {
		if in.store != nil {
			// Embed the real query so dimensionality (a 1-dim stub vs VECTOR(1024))
			// is exercised here, not deferred to pool/run.
			vec, err := in.store.QueryVector(ctx, q.Description)
			if err != nil {
				row.status = "EMBED_ERR"
				row.detail = truncate(err.Error(), 120)
				return row
			}
			queryVector = vec
		} else {
			// No embedder, and the kind doesn't require one — stub a vector so the
			// query parses, but say so rather than report a bare OK.
			queryVector = []float32{0}
			row.detail = "stubbed vector"
		}
	}
	resolved, err := q.ResolveEngineQuery(suite.ResolveOptions{
		Engine:      in.binding.QuerySource,
		Registry:    in.loaded.Registry,
		SuiteDir:    in.loaded.Dir,
		Defaults:    in.binding.Params,
		QueryVector: queryVector,
	})
	if err != nil {
		row.status = "TEMPLATE_ERR"
		row.detail = err.Error()
		return row
	}
	v, ok := exec.(engine.Validator)
	if !ok {
		row.status = "UNSUPPORTED"
		row.detail = "executor does not implement Validator"
		return row
	}
	validateCtx, cancel := context.WithTimeout(ctx, in.queryTimeout)
	defer cancel()
	if err := v.Validate(validateCtx, in.dialect.Request(in.track, resolved)); err != nil {
		row.status = "INVALID"
		row.detail = truncate(err.Error(), 120)
		return row
	}
	row.status = "OK"
	return row
}

// warnKindDrift cross-checks the declared kind against observed query usage so
// the two sources of truth can't silently diverge: a semantic/hybrid kind whose
// queries never take the query vector, or vector-bearing queries under a
// non-vector kind. Advisory only — it never fails the run.
func warnKindDrift(w io.Writer, bs *spec.BenchSpec, suites map[string]*suite.LoadedSuite) {
	anyNeedsVector := false
	for _, ls := range suites {
		for i := range ls.Suite.Queries {
			if ls.NeedsQueryVector(&ls.Suite.Queries[i]) {
				anyNeedsVector = true
			}
		}
	}
	switch {
	case bs.Kind.RequiresEmbedder() && !anyNeedsVector:
		printWarn(w, fmt.Sprintf("kind %q expects vector queries, but no query takes the %q arg",
			bs.Kind, suite.QueryVectorArg))
	case bs.Kind != "" && !bs.Kind.RequiresEmbedder() && anyNeedsVector:
		printWarn(w, fmt.Sprintf("queries take the %q arg but kind %q is not semantic/hybrid",
			suite.QueryVectorArg, bs.Kind))
	}
}

func printValidateRows(w io.Writer, rows []validateRow) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, cBold.Sprint("QUERY")+"\t"+cBold.Sprint("ENGINE")+"\t"+cBold.Sprint("STATUS")+"\t"+cBold.Sprint("DETAIL"))
	fmt.Fprintln(tw, "-----\t------\t------\t------")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.queryID, r.engine, colorStatus(r.status), r.detail)
	}
	tw.Flush()
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
