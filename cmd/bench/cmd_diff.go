package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/DjordjeVuckovic/tusker/internal/bench/report"
	"github.com/DjordjeVuckovic/tusker/internal/bench/trackctx"
	"github.com/spf13/cobra"
)

type diffFlags struct {
	trackArg string
	pathA    string
	pathB    string
}

func newDiffCmd() *cobra.Command {
	var f diffFlags
	cmd := &cobra.Command{
		Use:   "diff [track]",
		Short: "Compare two benchmark runs side by side",
		Long: `Loads two report JSON files and shows per-engine metric deltas and the
per-query NDCG regressions sorted by magnitude.

Without --b, B is the report latest.json points at. Without --a, A is the
newest earlier complete report (judged metrics, no failed queries, nothing
recorded in settings.incomplete), or the newest earlier report, with a
warning, when none is. --a and --b accept run IDs
(e.g. 2026-05-26T23-39-05-run-7e0750) or direct paths to report.json files.`,
		Example: `  bench diff tracks/global-news-dataset/fts_quality                               # latest two runs
  bench diff tracks/global-news-dataset/fts_quality --b 2026-05-26T23-39-05-run-7e0750
  bench diff --a /tmp/before.json --b /tmp/after.json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return executeDiff(cmd.OutOrStdout(), f, args)
		},
	}
	cmd.Flags().StringVar(&f.trackArg, "track", "", "Track path (relative to --track-root)")
	cmd.Flags().StringVar(&f.pathA, "a", "", "Before run: run ID or path to report.json")
	cmd.Flags().StringVar(&f.pathB, "b", "", "After run: run ID or path to report.json")
	return cmd
}

func executeDiff(w io.Writer, f diffFlags, args []string) error {
	tr, err := trackctx.Resolve(trackInputs(f.trackArg, args))
	if err != nil {
		// If both paths are explicit we don't need a track.
		if f.pathA != "" && f.pathB != "" {
			tr = nil
		} else {
			return err
		}
	}

	rptB, err := afterReport(tr, f.pathB)
	if err != nil {
		return fmt.Errorf("resolve after report: %w", err)
	}
	rptA, err := beforeReport(tr, f.pathA, rptB)
	if err != nil {
		return fmt.Errorf("resolve before report: %w", err)
	}
	if rptA.Provenance.RunID == rptB.Provenance.RunID {
		return fmt.Errorf("both runs resolve to the same report: %s", rptA.Provenance.RunID)
	}

	printDiff(w, rptA, rptB)
	return nil
}

// afterReport defaults to the report latest.json points at, or the newest
// fully scored report when there is no pointer.
func afterReport(tr *trackctx.Track, flag string) (*report.Report, error) {
	if flag != "" {
		return readExplicitReport(tr, flag)
	}
	if tr == nil {
		return nil, fmt.Errorf("no path or run ID given and no track available")
	}
	rpt, err := report.ReadLatestReport(tr.LatestReportPath())
	if errors.Is(err, fs.ErrNotExist) {
		return previousScoredReport(baselineSearch{ReportsDir: reportsDir(tr)})
	}
	return rpt, err
}

// beforeReport defaults to the newest fully scored report older than after.
func beforeReport(tr *trackctx.Track, flag string, after *report.Report) (*report.Report, error) {
	if flag != "" {
		return readExplicitReport(tr, flag)
	}
	if tr == nil {
		return nil, fmt.Errorf("no path or run ID given and no track available")
	}
	return previousScoredReport(baselineSearch{ReportsDir: reportsDir(tr), BeforeRunID: after.Provenance.RunID})
}

func reportsDir(tr *trackctx.Track) string {
	return filepath.Dir(tr.LatestReportPath())
}

// readExplicitReport reads a report named by a run ID or a path.
func readExplicitReport(tr *trackctx.Track, flag string) (*report.Report, error) {
	path := flag
	if !looksLikePath(flag) {
		if tr == nil {
			return nil, fmt.Errorf("run ID %q given but no track resolved", flag)
		}
		path = tr.ReportPath(flag)
	}
	return report.ReadJSON(path)
}

// baselineSearch scopes the hunt for a report to diff against.
type baselineSearch struct {
	ReportsDir string
	// BeforeRunID limits the search to older reports; empty searches them all.
	BeforeRunID string
}

// previousScoredReport returns the newest report in scope that is fully
// scored, falling back to the newest readable one when none is.
func previousScoredReport(search baselineSearch) (*report.Report, error) {
	dir := search.ReportsDir
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read reports dir %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && e.Name() != "latest.json" {
			names = append(names, e.Name())
		}
	}
	// Run IDs are timestamped, so lexicographic order is chronological.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	var newestReadable *report.Report
	for _, name := range names {
		if search.BeforeRunID != "" && strings.TrimSuffix(name, ".json") >= search.BeforeRunID {
			continue
		}
		rpt, err := report.ReadJSON(filepath.Join(dir, name))
		if err != nil {
			slog.Warn("skipping unreadable report in baseline search", "report", name, "error", err)
			continue
		}
		if fullyScored(rpt) {
			return rpt, nil
		}
		if newestReadable == nil {
			newestReadable = rpt
		}
	}
	if newestReadable == nil {
		return nil, fmt.Errorf("no earlier readable report in %s", dir)
	}
	slog.Warn("no earlier report is complete; diffing against the newest one",
		"report", newestReadable.Provenance.RunID)
	return newestReadable, nil
}

// fullyScored reports whether every job carries quality metrics, no
// (query, engine) cell failed and bench run recorded nothing incomplete. A
// latency-only, partial or off-spec run is not a baseline: its deltas would
// measure the failures, not the engines.
func fullyScored(r *report.Report) bool {
	if len(r.Jobs) == 0 || len(r.Settings.Incomplete) > 0 {
		return false
	}
	for _, job := range r.Jobs {
		judged := false
		for _, agg := range job.Aggregated {
			if agg.ErrorCount > 0 {
				return false
			}
			judged = judged || agg.JudgedCount > 0
		}
		for _, e := range job.PerQuery {
			if e.Error != "" {
				return false
			}
		}
		if !judged {
			return false
		}
	}
	return true
}

// ─── diff rendering ───────────────────────────────────────────────────────────

func printDiff(w io.Writer, a, b *report.Report) {
	fmt.Fprintf(w, "\n%s\n", cBold.Sprint("Comparing:"))
	fmt.Fprintf(w, "  %s %s\n", cDim.Sprint("A (before):"), a.Provenance.RunID)
	fmt.Fprintf(w, "  %s %s\n\n", cDim.Sprint("B (after): "), cBold.Sprint(b.Provenance.RunID))

	printProvenanceDiff(w, report.DiffProvenance(a, b))
	printScoringWarnings(w, a, b)

	// Find common jobs by name.
	jobsA := indexJobs(a)
	jobsB := indexJobs(b)

	names := unionKeys(jobsA, jobsB)
	sort.Strings(names)

	ndcgK, haveSharedK := largestSharedK(a.Config.KValues, b.Config.KValues)
	if !haveSharedK {
		fmt.Fprintf(w, "%s\n\n", cDim.Sprint("No K value in both runs: NDCG deltas skipped."))
	}

	for _, name := range names {
		jA, okA := jobsA[name]
		jB, okB := jobsB[name]
		if !okA || !okB {
			missing := "B"
			if !okA {
				missing = "A"
			}
			fmt.Fprintf(w, "%s\n", cDim.Sprintf("--- Job: %s (only in %s, skipped) ---", name, missing))
			continue
		}

		fmt.Fprintf(w, "%s\n\n", cBold.Sprintf("--- Job: %s ---", name))
		printAggregateDiff(w, jA, jB, ndcgK)
		if haveSharedK {
			printQueryDiff(w, jA, jB, ndcgK)
		}
		fmt.Fprintln(w)
	}
}

func printProvenanceDiff(w io.Writer, changes []report.ProvenanceChange) {
	if len(changes) == 0 {
		fmt.Fprintf(w, "%s\n\n", cDim.Sprint("Index provenance: unchanged"))
		return
	}
	fmt.Fprintf(w, "%s\n\n", cBold.Sprint("Index provenance changes:"))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "Engine\tField\tA\tB")
	fmt.Fprintln(tw, "---\t---\t---\t---")
	for _, c := range changes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Engine, c.Field, orAbsent(c.Before), orAbsent(c.After))
	}
	tw.Flush()
	fmt.Fprintln(w)
}

func orAbsent(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

// printAggregateDiff shows NDCG at ndcgK, or not at all when ndcgK is 0.
func printAggregateDiff(w io.Writer, jA, jB *report.JobReport, ndcgK int) {
	aggA := indexAgg(jA)
	aggB := indexAgg(jB)
	engines := unionKeys(aggA, aggB)
	sort.Strings(engines)

	ndcgHeader := "NDCG  A→B"
	if ndcgK > 0 {
		ndcgHeader = fmt.Sprintf("NDCG@%d (gain %s)  A→B", ndcgK, report.NDCGGain)
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "Engine\t%s\tMAP  A→B\tMRR  A→B\tp50  A→B\n", ndcgHeader)
	fmt.Fprintln(tw, "---\t---\t---\t---\t---")

	for _, eng := range engines {
		eA, okA := aggA[eng]
		eB, okB := aggB[eng]
		if !okA || !okB {
			fmt.Fprintf(tw, "%s\t(only in one run)\n", eng)
			continue
		}
		ndcg := "—"
		if ndcgK > 0 {
			ndcg = fmtDelta("%.4f", eA.NDCG[ndcgK], eB.NDCG[ndcgK], true)
		}
		mapA, mapB := eA.MAP, eB.MAP
		mrrA, mrrB := eA.MRR, eB.MRR
		latA, latB := eA.Latency.P50(), eB.Latency.P50()

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			eng,
			ndcg,
			fmtDelta("%.4f", mapA, mapB, true),
			fmtDelta("%.4f", mrrA, mrrB, true),
			fmtDurationDelta(latA, latB), // last column — safe to color
		)
	}
	tw.Flush()
	fmt.Fprintln(w)
}

func printQueryDiff(w io.Writer, jA, jB *report.JobReport, k int) {
	type delta struct {
		query, engine string
		a, b          float64
		diff          float64
	}

	perA := indexPerQuery(jA)
	perB := indexPerQuery(jB)

	var deltas []delta
	for key, eA := range perA {
		eB, ok := perB[key]
		if !ok {
			continue
		}
		ndcgA, scoredA := scoredNDCG(eA, k)
		ndcgB, scoredB := scoredNDCG(eB, k)
		if !scoredA || !scoredB {
			continue
		}
		d := ndcgB - ndcgA
		if d == 0 {
			continue
		}
		parts := strings.SplitN(key, "\x00", 2)
		deltas = append(deltas, delta{parts[0], parts[1], ndcgA, ndcgB, d})
	}
	if len(deltas) == 0 {
		return
	}
	// Sort: biggest regression first, then biggest improvement.
	sort.Slice(deltas, func(i, j int) bool { return deltas[i].diff < deltas[j].diff })

	fmt.Fprintf(w, "%s\n\n", cBold.Sprintf("Per-Query NDCG@%d (gain %s) Deltas (largest regression first):", k, report.NDCGGain))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "Query\tEngine\tA\tB\tΔ")
	fmt.Fprintln(tw, "---\t---\t---\t---\t---")
	for _, d := range deltas {
		// Δ is the last column — safe to color.
		var delta string
		if d.diff > 0 {
			delta = cOK.Sprintf("↑%+.4f", d.diff)
		} else {
			delta = cFail.Sprintf("↓%+.4f", d.diff)
		}
		fmt.Fprintf(tw, "%s\t%s\t%.4f\t%.4f\t%s\n",
			d.query, d.engine, d.a, d.b, delta)
	}
	tw.Flush()
}

// scoredNDCG is the NDCG@k a cell contributes to its engine's mean: a failed
// judged cell scores 0, as in the aggregates. Unjudged cells are not scored.
func scoredNDCG(e report.Entry, k int) (float64, bool) {
	if e.Error != "" {
		return 0, e.Judged
	}
	return e.NDCG[k], e.Judged
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func indexJobs(r *report.Report) map[string]*report.JobReport {
	m := make(map[string]*report.JobReport, len(r.Jobs))
	for i := range r.Jobs {
		m[r.Jobs[i].JobName] = &r.Jobs[i]
	}
	return m
}

func indexAgg(jr *report.JobReport) map[string]report.AggregatedEntry {
	m := make(map[string]report.AggregatedEntry, len(jr.Aggregated))
	for _, a := range jr.Aggregated {
		m[a.EngineName] = a
	}
	return m
}

func indexPerQuery(jr *report.JobReport) map[string]report.Entry {
	m := make(map[string]report.Entry, len(jr.PerQuery))
	for _, e := range jr.PerQuery {
		key := e.QueryID + "\x00" + e.EngineName
		m[key] = e
	}
	return m
}

func unionKeys[V any](a, b map[string]V) []string {
	seen := make(map[string]struct{})
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

func fmtDelta(format string, a, b float64, higherBetter bool) string {
	d := b - a
	if d == 0 {
		return fmt.Sprintf(format+" (=)", a)
	}
	improved := (d > 0) == higherBetter
	arrow := "↑"
	if !improved {
		arrow = "↓"
	}
	return fmt.Sprintf(format+"→"+format+" %s%s", a, b, arrow, fmtRelativeChange(d, a))
}

// fmtRelativeChange has no percentage to give for a change from zero.
func fmtRelativeChange(change, baseline float64) string {
	if baseline == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%%", change/baseline*100)
}

func fmtDurationDelta(a, b time.Duration) string {
	if a == 0 && b == 0 {
		return "—"
	}
	d := b - a
	if d == 0 {
		return cDim.Sprint(durStr(a) + " (=)")
	}
	// For latency: lower is better, so improvement = d < 0.
	improved := d < 0
	arrow := "↑" // up = slower = worse
	if improved {
		arrow = "↓" // down = faster = better
	}
	s := fmt.Sprintf("%s→%s %s%s", durStr(a), durStr(b), arrow, fmtRelativeChange(float64(d), float64(a)))
	if improved {
		return cOK.Sprint(s)
	}
	return cFail.Sprint(s)
}

// largestSharedK is the largest cutoff both runs computed. A K only one run
// has would read as a score of 0 on the other side.
func largestSharedK(a, b []int) (int, bool) {
	shared := 0
	for _, k := range a {
		if slices.Contains(b, k) && k > shared {
			shared = k
		}
	}
	return shared, shared > 0
}

// printScoringWarnings flags runs whose metrics were computed against different
// grades, where a delta measures the judging as much as the engine.
func printScoringWarnings(w io.Writer, a, b *report.Report) {
	var differences []string
	if judgeOf(a) != judgeOf(b) {
		differences = append(differences, fmt.Sprintf("judge: %s → %s", judgeOf(a), judgeOf(b)))
	}
	if a.Config.RelevanceThreshold != b.Config.RelevanceThreshold {
		differences = append(differences, fmt.Sprintf("relevance threshold: grade>=%d → grade>=%d",
			a.Config.RelevanceThreshold, b.Config.RelevanceThreshold))
	}
	if judgmentsOf(a) != judgmentsOf(b) {
		differences = append(differences, fmt.Sprintf("judgments: %s → %s", judgmentsOf(a), judgmentsOf(b)))
	}
	if len(differences) == 0 {
		return
	}
	fmt.Fprintf(w, "%s the runs were scored differently, so metric deltas are not engine changes alone:\n", cWarn.Sprint("WARNING:"))
	for _, d := range differences {
		fmt.Fprintf(w, "  %s\n", d)
	}
	fmt.Fprintln(w)
}

const scoringNotRecorded = "not recorded"

func judgeOf(r *report.Report) string {
	judge := r.Provenance.Judge
	if judge == nil {
		return scoringNotRecorded
	}
	if judge.PromptVersion == "" {
		return judge.String()
	}
	return judge.String() + " prompt " + judge.PromptVersion
}

// judgmentsOf names the judgments file relative to the track, so the same
// file in a moved track or another worktree compares equal. Reports record no
// digest of the file, so one regraded in place still compares equal.
func judgmentsOf(r *report.Report) string {
	sources := r.Provenance.Sources
	if sources == nil || sources.Judgments == "" {
		return scoringNotRecorded
	}
	if sources.Spec != "" {
		if rel, err := filepath.Rel(filepath.Dir(sources.Spec), sources.Judgments); err == nil {
			return rel
		}
	}
	return filepath.Clean(sources.Judgments)
}

func durStr(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%.1fµs", float64(d.Microseconds()))
	}
	if d < time.Second {
		return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}
