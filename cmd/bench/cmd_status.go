package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tadjov/tusker/internal/bench/trackctx"
)

func newStatusCmd() *cobra.Command {
	var trackArg string
	cmd := &cobra.Command{
		Use:   "status [track]",
		Short: "Show what has been generated for a track",
		Long: `Prints a one-glance summary of a track's pipeline state: which artifacts
exist, when they were last generated, and what the next natural step is.

Analogous to git status — run it at the start of any session to see where
you left off.`,
		Example: `  bench status tracks/global-news-dataset/fts_quality
  bench status          # walk-up from CWD`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return executeStatus(cmd.Context(), cmd.OutOrStdout(), trackArg, args)
		},
	}
	cmd.Flags().StringVar(&trackArg, "track", "", "Track path (relative to --track-root)")
	return cmd
}

func executeStatus(ctx context.Context, w io.Writer, track string, args []string) error {
	return forEachTrack(ctx, w, trackInputs(track, args), func(tr *trackctx.Track) error {
		return statusTrack(w, tr)
	})
}

func statusTrack(w io.Writer, tr *trackctx.Track) error {
	fmt.Fprintf(w, "%s %s\n", cBold.Sprint("Track:"), cBold.Sprint(tr.Name()))
	fmt.Fprintf(w, "  %s %s\n\n", cDim.Sprint("root:"), tr.Root)

	trecDir := filepath.Dir(tr.Pool)

	// Pool
	printArtifact(w, "Pool", tr.Pool, func(path string) string {
		return readPoolMeta(path)
	})

	// Judgments — glob for all annotations.*.yaml files.
	annotationGlob := filepath.Join(trecDir, "annotations.*.yaml")
	annotations, _ := filepath.Glob(annotationGlob)
	if len(annotations) == 0 {
		fmt.Fprintf(w, "  Judgments    %s  %s\n",
			cFail.Sprint("✗"),
			cDim.Sprintf("none found — run: bench judge %s --strategy keyword", tr.Name()))
	} else {
		for i, path := range annotations {
			prefix := "  Judgments   "
			if i > 0 {
				prefix = "               "
			}
			strategy := extractStrategy(path)
			printArtifact(w, prefix, path, func(p string) string {
				return readJudgmentMeta(p, strategy)
			})
		}
	}

	// Latest report
	latest := tr.LatestReportPath()
	if _, err := os.Stat(latest); os.IsNotExist(err) {
		fmt.Fprintf(w, "  Reports      %s  %s\n",
			cFail.Sprint("✗"),
			cDim.Sprintf("none — run: bench run %s", tr.Name()))
	} else {
		printArtifact(w, "Reports", latest, func(path string) string {
			return readLatestMeta(path)
		})
	}

	// Suggest next step.
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s  %s\n",
		cDim.Sprint("Next steps:"),
		cDim.Sprint("bench validate → bench pool → bench judge → bench run"))
	return nil
}

func printArtifact(w io.Writer, label, path string, describe func(string) string) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Fprintf(w, "  %-14s %s  %s\n", label, cFail.Sprint("✗"), cDim.Sprint(filepath.Base(path)+" (not found)"))
		return
	}
	desc := describe(path)
	fmt.Fprintf(w, "  %-14s %s  %s\n", label, cOK.Sprint("✓"), desc)
}

// extractStrategy pulls the strategy name out of "annotations.<strategy>.yaml".
func extractStrategy(path string) string {
	base := filepath.Base(path)
	base = strings.TrimPrefix(base, "annotations.")
	base = strings.TrimSuffix(base, ".yaml")
	return base
}

// ─── meta readers (partial JSON/YAML unmarshal to avoid full parse) ──────────

func readPoolMeta(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return filepath.Base(path)
	}
	// Pool is YAML — use a simple grep for the fields we need.
	info, _ := os.Stat(path)
	mtime := ""
	if info != nil {
		mtime = info.ModTime().Format("2006-01-02")
	}
	// Count "- query_id:" entries as proxy for query count.
	queryCount := strings.Count(string(data), "query_id:")
	return fmt.Sprintf("trec/pool.yaml  %s  queries≈%d", mtime, queryCount)
}

func readJudgmentMeta(path, strategy string) string {
	info, _ := os.Stat(path)
	mtime := ""
	if info != nil {
		mtime = info.ModTime().Format("2006-01-02")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("annotations.%s.yaml  %s", strategy, mtime)
	}
	gradedCount := strings.Count(string(data), "grade:")
	return fmt.Sprintf("annotations.%s.yaml  %s  graded≈%d", strategy, mtime, gradedCount)
}

type latestPtr struct {
	Latest string `json:"latest"`
}

func readLatestMeta(latestPath string) string {
	data, err := os.ReadFile(latestPath)
	if err != nil {
		return "reports/latest.json (unreadable)"
	}
	var ptr latestPtr
	if err := json.Unmarshal(data, &ptr); err != nil || ptr.Latest == "" {
		return "reports/latest.json (malformed)"
	}
	reportPath := ptr.Latest
	if !filepath.IsAbs(reportPath) {
		reportPath = filepath.Join(filepath.Dir(latestPath), reportPath)
	}

	info, _ := os.Stat(reportPath)
	mtime := ""
	if info != nil {
		mtime = info.ModTime().Format("2006-01-02 15:04")
	}

	// Read just the run_id from the report JSON without full parse.
	rdata, err := os.ReadFile(reportPath)
	if err != nil {
		return fmt.Sprintf("latest → %s  %s", filepath.Base(reportPath), mtime)
	}
	type miniReport struct {
		Provenance struct {
			RunID string `json:"RunID"`
		} `json:"provenance"`
	}
	var mini miniReport
	_ = json.Unmarshal(rdata, &mini)
	runID := mini.Provenance.RunID
	if runID == "" {
		runID = filepath.Base(reportPath)
	}
	return fmt.Sprintf("latest → %s  (%s)", runID, mtime)
}
