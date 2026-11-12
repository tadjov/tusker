package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tadjov/tusker/internal/bench/judgment"
	"github.com/tadjov/tusker/internal/bench/report"
	"github.com/tadjov/tusker/internal/bench/trackctx"
)

type exportFlags struct {
	trackArg      string
	format        string
	strategy      string
	judgmentsPath string
	output        string
}

func newExportCmd() *cobra.Command {
	var f exportFlags
	cmd := &cobra.Command{
		Use:   "export [track]",
		Short: "Export benchmark artifacts to TSV or HTML",
		Long: `Exports bench artifacts in a machine-readable or presentation format.

  --format qrels     TREC qrels TSV (query_id 0 doc_id grade) for trec_eval, R, Python
  --format tsv       Alias for qrels
  --format html      Self-contained HTML report with sortable tables, SVG charts,
                     significance table, and provenance block — suitable for thesis
                     appendices or sharing without tooling.
  --format markdown  GitHub-Flavored Markdown tables for thesis writing and PRs
  --format run       TREC run files (qid Q0 docid rank score tag), one per job and
                     engine, written to --output or reports/<run_id>.run/

qrels/tsv picks the judgments file in this order: --judgments PATH > --strategy NAME >
defaults to keyword. Without --output the qrels go to qrels.<name>.tsv beside the
judgments file. html, markdown and run read the track's latest report.`,
		Example: `  bench export tracks/global-news-dataset/fts_quality --format qrels
  bench export tracks/global-news-dataset/fts_quality --format qrels --strategy claude-api
  bench export tracks/global-news-dataset/fts_quality --format html
  bench export tracks/global-news-dataset/fts_quality --format markdown
  bench export tracks/global-news-dataset/fts_quality --format markdown --output /tmp/results.md
  bench export tracks/global-news-dataset/fts_quality --format run
  bench export --judgments /tmp/ad-hoc.yaml --format qrels --output /tmp/q.tsv`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return executeExport(cmd, f, args)
		},
	}
	cmd.Flags().StringVar(&f.trackArg, "track", "", "Track path (relative to --track-root)")
	cmd.Flags().StringVar(&f.format, "format", "qrels", "Export format: qrels (TREC TSV), html, markdown, or run (TREC run files)")
	cmd.Flags().StringVar(&f.strategy, "strategy", string(judgment.StrategyKeyword), "Judgment strategy for TSV export")
	cmd.Flags().StringVar(&f.judgmentsPath, "judgments", "", "Override annotations YAML path (TSV only)")
	cmd.Flags().StringVar(&f.output, "output", "", "Override output path")
	return cmd
}

func executeExport(cmd *cobra.Command, f exportFlags, args []string) error {
	switch strings.ToLower(f.format) {
	case "qrels", "tsv":
		return exportTSV(cmd, f, args)
	case "html":
		return exportHTML(cmd, f, args)
	case "markdown", "md":
		return exportMarkdown(cmd, f, args)
	case "run":
		return exportRun(cmd, f, args)
	default:
		return fmt.Errorf("unknown format %q (choose qrels, html, markdown, or run)", f.format)
	}
}

func exportTSV(cmd *cobra.Command, f exportFlags, args []string) error {
	in := trackInputs(f.trackArg, args)
	in.OutputPath = f.output
	tr, err := trackctx.Resolve(in)
	if err != nil {
		return err
	}

	jPath, qrelsName := f.judgmentsPath, f.judgmentsPath
	if jPath == "" {
		jPath, qrelsName = tr.JudgmentsPath(f.strategy), f.strategy
	}

	outPath := f.output
	if outPath == "" {
		outPath = tr.QrelsPath(qrelsName)
	}

	if samePath(jPath, outPath) {
		return fmt.Errorf("qrels output %s is the judgments file it is exported from; pass a different --output", outPath)
	}

	jf, err := judgment.ReadFile(jPath)
	if err != nil {
		return fmt.Errorf("read judgments %s: %w", jPath, err)
	}
	if err := writeQrelsAtomic(jf, outPath); err != nil {
		return fmt.Errorf("write qrels %s: %w", outPath, err)
	}
	printDone(cmd.OutOrStdout(), "Qrels written: "+outPath)
	return nil
}

// writeQrelsAtomic writes the qrels beside path and renames them over it, so a
// reader of path sees the old file or the new one, never a partial write.
func writeQrelsAtomic(jf *judgment.File, path string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".qrels-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := judgment.WriteQrels(jf, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}

// samePath reports whether a and b name one file, through symlinked
// directories too. b need not exist yet.
func samePath(a, b string) bool {
	return resolvedPath(a) == resolvedPath(b)
}

func resolvedPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(dir, filepath.Base(abs))
	}
	return abs
}

func exportMarkdown(cmd *cobra.Command, f exportFlags, args []string) error {
	tr, err := trackctx.Resolve(trackInputs(f.trackArg, args))
	if err != nil {
		return err
	}

	rpt, err := report.ReadLatestReport(tr.LatestReportPath())
	if err != nil {
		return fmt.Errorf("read latest report: %w", err)
	}

	outPath := f.output
	if outPath == "" {
		runID := rpt.Provenance.RunID
		if runID == "" {
			runID = "report"
		}
		outPath = filepath.Join(filepath.Dir(tr.LatestReportPath()), runID+".md")
	}

	f2, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create markdown file: %w", err)
	}
	defer f2.Close()

	report.WriteMarkdown(rpt, f2)
	printDone(cmd.OutOrStdout(), "Markdown report written: "+outPath)
	return nil
}

func exportHTML(cmd *cobra.Command, f exportFlags, args []string) error {
	tr, err := trackctx.Resolve(trackInputs(f.trackArg, args))
	if err != nil {
		return err
	}

	rpt, err := report.ReadLatestReport(tr.LatestReportPath())
	if err != nil {
		return fmt.Errorf("read latest report: %w", err)
	}

	outPath := f.output
	if outPath == "" {
		// Derive HTML path from the run ID: reports/<run_id>.html
		runID := rpt.Provenance.RunID
		if runID == "" {
			runID = "report"
		}
		outPath = filepath.Join(filepath.Dir(tr.LatestReportPath()), runID+".html")
	}

	if err := report.WriteHTML(rpt, outPath); err != nil {
		return fmt.Errorf("write HTML report: %w", err)
	}
	printDone(cmd.OutOrStdout(), "HTML report written: "+outPath)
	return nil
}

// exportRun writes the latest report's rankings as TREC run files, one per job
// and engine, into --output or reports/<run_id>.run/.
func exportRun(cmd *cobra.Command, f exportFlags, args []string) error {
	tr, err := trackctx.Resolve(trackInputs(f.trackArg, args))
	if err != nil {
		return err
	}

	rpt, err := report.ReadLatestReport(tr.LatestReportPath())
	if err != nil {
		return fmt.Errorf("read latest report: %w", err)
	}

	outDir := f.output
	if outDir == "" {
		runID := rpt.Provenance.RunID
		if runID == "" {
			runID = "report"
		}
		outDir = filepath.Join(filepath.Dir(tr.LatestReportPath()), runID+".run")
	}

	paths, err := report.WriteTRECRuns(rpt, outDir)
	if err != nil {
		return fmt.Errorf("write TREC runs: %w", err)
	}
	for _, path := range paths {
		printDone(cmd.OutOrStdout(), "TREC run written: "+path)
	}
	return nil
}
