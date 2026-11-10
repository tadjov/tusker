package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/DjordjeVuckovic/tusker/internal/bench/judgment"
	"github.com/DjordjeVuckovic/tusker/internal/bench/pool"
	"github.com/DjordjeVuckovic/tusker/internal/bench/report"
	"github.com/DjordjeVuckovic/tusker/internal/bench/spec"
	"github.com/DjordjeVuckovic/tusker/internal/bench/trackctx"
	"github.com/spf13/cobra"
)

// newReportCmd exposes "bench report" as a top-level shortcut for the common
// case of "bench show report". Same RunE, different Use/Short.
func newReportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "report [track|path]",
		Short: "Show the latest report for a track (alias: bench show report)",
		Long: `Shortcut for bench show report. Prints provenance + aggregated metrics +
latency + significance table for the most-recent run of the given track.`,
		Args:    cobra.MaximumNArgs(1),
		Example: "  bench report tracks/global-news-dataset/fts_quality\n  bench report /path/to/report.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			var rpt *report.Report
			var err error
			if len(args) == 1 && looksLikePath(args[0]) {
				rpt, err = report.ReadJSON(args[0])
			} else {
				in := trackctx.Inputs{TrackRoot: trackRoot}
				if len(args) == 1 {
					in.TrackArg = args[0]
				}
				tr, rerr := trackctx.Resolve(in)
				if rerr != nil {
					return rerr
				}
				rpt, err = report.ReadLatestReport(tr.LatestReportPath())
			}
			if err != nil {
				return err
			}
			showReport(cmd.OutOrStdout(), rpt)
			return nil
		},
	}
}

func newShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Inspect bench artifacts (pool, judgments, spec)",
		Long: `Pretty-prints a one-page summary of a bench artifact: query counts, grade
histograms, engine coverage, dedup ratios. The single best way to sanity-check
intermediates without grepping YAML.`,
	}
	cmd.AddCommand(newShowPoolCmd(), newShowJudgmentsCmd(), newShowSpecCmd(), newShowReportCmd())
	return cmd
}

func newShowPoolCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "pool [track|path]",
		Short:   "Summarise a pool file",
		Args:    cobra.MaximumNArgs(1),
		Example: "  bench show pool tracks/global-news-dataset/fts_quality\n  bench show pool /path/to/pool.yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveArtifactPath(args, "pool", "")
			if err != nil {
				return err
			}
			pf, err := pool.ReadPoolFile(path)
			if err != nil {
				return err
			}
			showPool(cmd.OutOrStdout(), pf)
			return nil
		},
	}
}

func newShowJudgmentsCmd() *cobra.Command {
	var strategy string
	cmd := &cobra.Command{
		Use:     "judgments [track|path]",
		Short:   "Summarise a judgments file",
		Args:    cobra.MaximumNArgs(1),
		Example: "  bench show judgments tracks/global-news-dataset/fts_quality --strategy claude-api\n  bench show judgments /path/to/ann.yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			s := strategy
			if s == "" {
				s = string(judgment.StrategyLexical)
			}
			path, err := resolveArtifactPath(args, "judgments", s)
			if err != nil {
				return err
			}
			jf, err := judgment.ReadFile(path)
			if err != nil {
				return err
			}
			showJudgments(cmd.OutOrStdout(), jf)
			return nil
		},
	}
	cmd.Flags().StringVar(&strategy, "strategy", "", "Strategy name when summarising by track (default: lexical)")
	return cmd
}

func newShowSpecCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "spec [track|path]",
		Short:   "Summarise a bench spec",
		Args:    cobra.MaximumNArgs(1),
		Example: "  bench show spec tracks/global-news-dataset/fts_quality\n  bench show spec /path/to/spec.yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveArtifactPath(args, "spec", "")
			if err != nil {
				return err
			}
			bs, err := spec.LoadFromFile(path)
			if err != nil {
				return err
			}
			showSpec(cmd.OutOrStdout(), bs)
			return nil
		},
	}
}

func newShowReportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "report [track|path]",
		Short: "Summarise the latest (or given) report",
		Long: `Pretty-prints a one-page summary of a report JSON file: provenance block
(run_id, tool, spec_id, sources) followed by the aggregated metrics and latency
tables for each job.

With a track name or path, follows reports/latest.json to the actual report.
With a direct .json path, reads that file.`,
		Args:    cobra.MaximumNArgs(1),
		Example: "  bench show report tracks/global-news-dataset/fts_quality\n  bench show report /path/to/report.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			var rpt *report.Report
			var err error

			if len(args) == 1 && looksLikePath(args[0]) {
				rpt, err = report.ReadJSON(args[0])
			} else {
				in := trackctx.Inputs{TrackRoot: trackRoot}
				if len(args) == 1 {
					in.TrackArg = args[0]
				}
				tr, rerr := trackctx.Resolve(in)
				if rerr != nil {
					return rerr
				}
				rpt, err = report.ReadLatestReport(tr.LatestReportPath())
			}
			if err != nil {
				return err
			}
			showReport(cmd.OutOrStdout(), rpt)
			return nil
		},
	}
}

// resolveArtifactPath turns the positional arg (track name OR direct path)
// into the on-disk artifact path. If args is empty, walk-up CWD detection
// decides. kind is one of: spec, pool, judgments. strategy applies only to
// judgments.
func resolveArtifactPath(args []string, kind, strategy string) (string, error) {
	if len(args) == 1 && looksLikePath(args[0]) {
		// Treat any path-shaped arg as a direct path. trackctx.Resolve would
		// reject it as non-track-shaped, but for `show` we don't care — the
		// caller just wants to read whatever file the user pointed at.
		return args[0], nil
	}
	in := trackctx.Inputs{TrackRoot: trackRoot}
	if len(args) == 1 {
		in.TrackArg = args[0]
	}
	tr, err := trackctx.Resolve(in)
	if err != nil {
		return "", err
	}
	switch kind {
	case "spec":
		return tr.Spec, nil
	case "pool":
		return tr.Pool, nil
	case "judgments":
		return tr.JudgmentsPath(strategy), nil
	default:
		return "", fmt.Errorf("unknown artifact kind %q", kind)
	}
}

func looksLikePath(s string) bool {
	if s == "" {
		return false
	}
	if len(s) > 0 && (s[0] == '/' || s[0] == '.') {
		return true
	}
	for _, r := range s {
		if r == '/' {
			return true
		}
	}
	// Treat anything with a recognised extension as a path.
	for _, ext := range []string{".yaml", ".yml", ".tsv", ".json"} {
		if hasSuffix(s, ext) {
			return true
		}
	}
	return false
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func showPool(w io.Writer, pf *pool.PoolFile) {
	fmt.Fprintf(w, "Pool: %s\n", pf.SuiteName)
	fmt.Fprintf(w, "Queries: %d\n\n", len(pf.Queries))

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "QUERY\tDOCS\tSOURCES\tDEDUP")
	fmt.Fprintln(tw, "-----\t----\t-------\t-----")

	var totalDocs, totalSourceHits int
	engineCounts := map[string]int{}

	for _, e := range pf.Queries {
		sources := map[string]int{}
		sourceHits := 0
		for _, d := range e.Docs {
			for _, s := range d.Sources {
				sources[s]++
				sourceHits++
				engineCounts[s]++
			}
		}
		totalDocs += len(e.Docs)
		totalSourceHits += sourceHits

		dedup := "—"
		if sourceHits > 0 {
			dedup = fmt.Sprintf("%.0f%%", 100*float64(sourceHits-len(e.Docs))/float64(sourceHits))
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", e.QueryID, len(e.Docs), formatSources(sources), dedup)
	}
	tw.Flush()

	fmt.Fprintf(w, "\nTotal unique docs: %d\n", totalDocs)
	fmt.Fprintf(w, "Total engine hits: %d\n", totalSourceHits)
	fmt.Fprintln(w, "\nPer-engine contribution:")
	for _, e := range sortedKeys(engineCounts) {
		fmt.Fprintf(w, "  %s: %d docs\n", e, engineCounts[e])
	}
}

func showJudgments(w io.Writer, jf *judgment.File) {
	fmt.Fprintf(w, "Judgments (judge=%s)\n", jf.Judge())
	fmt.Fprintf(w, "Queries: %d\n\n", len(jf.Queries))

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "QUERY\tTOTAL\tGRADED\tUNJUDGED\t3 (HI)\t2 (REL)\t1 (MARG)\t0 (NO)")
	fmt.Fprintln(tw, "-----\t-----\t------\t--------\t------\t-------\t--------\t------")

	totals := map[int]int{}
	allTotal, allGraded, allUnjudged := 0, 0, 0

	for _, qe := range jf.Queries {
		h := map[int]int{}
		graded, unjudged := 0, 0
		for _, d := range qe.Docs {
			if d.Grade < 0 {
				unjudged++
				continue
			}
			graded++
			h[d.Grade]++
			totals[d.Grade]++
		}
		allTotal += len(qe.Docs)
		allGraded += graded
		allUnjudged += unjudged
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
			qe.QueryID, len(qe.Docs), graded, unjudged, h[3], h[2], h[1], h[0])
	}
	tw.Flush()

	fmt.Fprintf(w, "\nTotal: %d docs across %d queries\n", allTotal, len(jf.Queries))
	fmt.Fprintf(w, "Graded: %d  Unjudged: %d\n", allGraded, allUnjudged)
	fmt.Fprintf(w, "Distribution: 3=%d  2=%d  1=%d  0=%d\n", totals[3], totals[2], totals[1], totals[0])
}

func showSpec(w io.Writer, bs *spec.BenchSpec) {
	fmt.Fprintln(w, "Engines:")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tTYPE\tCONNECTION\tINDEX\tQUERIES FROM\tPARAMS")
	for _, name := range sortedSpecKeys(bs.Engines) {
		e := bs.Engines[name]
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			name, e.Type, maskConn(e.Connection), e.Index, e.QueriesFrom, formatEngineParams(e.Params))
	}
	tw.Flush()

	fmt.Fprintln(w, "\nJobs:")
	for _, j := range bs.Jobs {
		fmt.Fprintf(w, "  %s\n    suite:   %s\n    engines: %v\n", j.Name, j.Suite, j.Engines)
	}

	fmt.Fprintf(w, "\nMetrics: k=%v threshold=%d\n",
		bs.Metrics.KValues, bs.Metrics.RelevanceThreshold)
	fmt.Fprintf(w, "Runs: warmup=%d iterations=%d\n", bs.Runs.Warmup, bs.Runs.Iterations)
}

func showReport(w io.Writer, rpt *report.Report) {
	p := rpt.Provenance
	fmt.Fprintf(w, "Run ID:    %s\n", p.RunID)
	fmt.Fprintf(w, "Tool:      %s\n", p.Tool)
	fmt.Fprintf(w, "Generated: %s\n", p.GeneratedAt.Format("2006-01-02 15:04:05 UTC"))
	if p.SpecID != "" {
		fmt.Fprintf(w, "Spec:      %s\n", p.SpecID)
	}
	if p.Sources != nil {
		fmt.Fprintln(w, "\nSources:")
		if p.Sources.Spec != "" {
			fmt.Fprintf(w, "  spec:      %s\n", p.Sources.Spec)
		}
		if len(p.Sources.Suites) > 0 {
			fmt.Fprintf(w, "  suites:    %s\n", strings.Join(p.Sources.Suites, ", "))
		}
		if p.Sources.Pool != "" {
			fmt.Fprintf(w, "  pool:      %s\n", p.Sources.Pool)
		}
		if p.Sources.Judgments != "" {
			fmt.Fprintf(w, "  judgments: %s\n", p.Sources.Judgments)
		}
	}
	showEngineIndexes(w, rpt.Environment.Engines)
	fmt.Fprintln(w)
	report.WriteTable(rpt, w)
}

func showEngineIndexes(w io.Writer, engines map[string]report.EngineInfo) {
	if len(engines) == 0 {
		return
	}
	names := make([]string, 0, len(engines))
	for name := range engines {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintln(w, "\nEngines:")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, name := range names {
		info := engines[name]
		summary := "index provenance not recorded"
		if info.Index != nil {
			summary = info.Index.Summary()
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", name, info.Type, summary)
	}
	tw.Flush()
}

func formatSources(sources map[string]int) string {
	if len(sources) == 0 {
		return "—"
	}
	keys := sortedKeys(sources)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%s:%d", k, sources[k])
	}
	return out
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedSpecKeys(m map[string]spec.Engine) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// formatEngineParams renders engine params key-sorted so two arms of an A/B can
// be compared line by line.
func formatEngineParams(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = fmt.Sprintf("%s=%v", k, params[k])
	}
	return strings.Join(pairs, " ")
}

func maskConn(s string) string {
	if len(s) > 60 {
		return s[:30] + "…" + s[len(s)-15:]
	}
	return s
}
