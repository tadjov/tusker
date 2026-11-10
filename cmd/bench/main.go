package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/DjordjeVuckovic/tusker/internal/bench/judgment"
	"github.com/DjordjeVuckovic/tusker/internal/bench/spec"
	"github.com/spf13/cobra"
)

func init() {
	// Let the spec loader query the strategy registry without importing
	// judgment directly (keeps spec package dep-free of judgment).
	spec.KnownStrategies = judgment.KnownStrategies
}

const (
	cliName  = "bench"
	cliShort = "Search engine quality + latency benchmark"
	cliLong  = `bench evaluates full-text, vector, and hybrid search queries against multiple
engines (Postgres, ParadeDB, Elasticsearch, the tusker API), produces
IR-quality metrics (NDCG, MAP, MRR, Bpref, P/R/F1) and latency statistics.

Typical pipeline (pass the track path as a positional arg):

  1. bench init <track>              scaffold a track folder
  2. bench validate <track>          dry-run every query through each engine
  3. bench pool <track>              gather candidate docs → trec/pool.yaml
  4. bench judge <track>             grade with lexical (default) or LLM strategy
  5. bench run <track>               execute + report (reads spec.defaults.judgments)
  6. bench export <track> --format html    shareable HTML report
     bench export <track> --format qrels  TREC qrels for trec_eval / R / Python

  bench status <track>               see where you left off
  bench diff   <track>               compare latest two runs
  bench clean  <track>               remove old report files (keep N newest)

A track is any folder holding spec.yaml + suite.yaml + trec/ — nothing above it
is assumed, so tracks/global-news-dataset/fts_quality and benches/b1 are both
fine. The arg is an ordinary path: absolute, or relative to the track root.
validate/pool/judge/run/status also take a glob — quote it:
bench run 'tracks/global-news-dataset/*'.

--track-root <dir> (or BENCH_TRACK_ROOT) is where relative track paths start,
defaulting to the current directory.

  BENCH_TRACK_ROOT=tracks/global-news-dataset bench run news_fuzzy
  BENCH_TRACK_ROOT=tracks/global-news-dataset bench run '*'
`
)

func main() {
	root := &cobra.Command{
		Use:           cliName,
		Short:         cliShort,
		Long:          cliLong,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&trackRoot, "track-root", os.Getenv(trackRootEnv),
		"Base directory for relative track paths (default $"+trackRootEnv+", else cwd)")
	root.AddCommand(
		newRunCmd(),
		newPoolCmd(),
		newJudgeCmd(),
		newGradeCmd(),
		newValidateCmd(),
		newInitCmd(),
		newShowCmd(),
		newExportCmd(),
		newStatusCmd(),
		newDiffCmd(),
		newCleanCmd(),
		newReportCmd(), // top-level alias for bench show report
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	if err := root.ExecuteContext(ctx); err != nil {
		// Printed rather than logged: resolution errors carry indented hint
		// lines, and slog would escape the newlines into one unreadable string.
		fmt.Fprintf(os.Stderr, "%s %v\n", cFail.Sprint("Error:"), err)
		os.Exit(1)
	}
}
