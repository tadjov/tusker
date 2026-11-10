package report

import (
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/DjordjeVuckovic/tusker/internal/bench/metrics"
	"github.com/fatih/color"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

// table-level color instances
var (
	tBold   = color.New(color.Bold)
	tGreen  = color.New(color.FgGreen, color.Bold)
	tRed    = color.New(color.FgRed, color.Bold)
	tYellow = color.New(color.FgYellow)
	tDim    = color.New(color.FgHiBlack)
)

func WriteTable(r *Report, w io.Writer) {
	title := r.Provenance.SpecID
	if title == "" {
		title = "Benchmark"
	}
	fmt.Fprintf(w, "\n%s\n", tBold.Sprintf("=== %s  run_id=%s ===", title, r.Provenance.RunID))
	writeScoringHeader(w, r)

	for _, jr := range r.Jobs {
		fmt.Fprintf(w, "\n%s\n", tBold.Sprintf("--- Job: %s ---", jr.JobName))
		if !hasAnyJudgments(&jr) {
			fmt.Fprintf(w, "\n%s No relevance judgments found. Showing latency only.\n",
				tYellow.Sprint("WARNING:"))
			fmt.Fprintf(w, "  Run bench pool, then bench judge.\n\n")
			writeLatencyTable(w, &jr)
		} else {
			writeAggregatedTable(w, &jr, r.Config.KValues)
			writeCategoryTables(w, &jr, r.Config.KValues)
			writeLatencyTable(w, &jr)
			writeSignificanceTable(w, &jr)
			writePerQueryTable(w, &jr, r.Config.KValues)
		}
	}
}

// The judge and the cutoff change what every number below them means.
func writeScoringHeader(w io.Writer, r *Report) {
	judge := "unknown"
	if r.Provenance.Judge != nil {
		judge = r.Provenance.Judge.String()
	}
	fmt.Fprintf(w, "%s\n", tDim.Sprintf("judge=%s  relevant=grade>=%d", judge, r.Config.RelevanceThreshold))
}

func hasAnyJudgments(jr *JobReport) bool {
	for _, agg := range jr.Aggregated {
		if agg.JudgedCount > 0 {
			return true
		}
	}
	for _, e := range jr.PerQuery {
		if e.Judged {
			return true
		}
	}
	return false
}

// fmtRecorded renders a metric older reports never recorded as "—", not 0.
func fmtRecorded(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.4f", *v)
}

// WriteMarkdown renders the report as GitHub-Flavored Markdown suitable for
// thesis appendices and pull request descriptions. Color is stripped; tables
// are rendered in GFM pipe format via go-pretty's RenderMarkdown().
func WriteMarkdown(r *Report, w io.Writer) {
	title := r.Provenance.SpecID
	if title == "" {
		title = "Benchmark"
	}
	fmt.Fprintf(w, "# %s\n\n", title)
	fmt.Fprintf(w, "**run_id:** `%s`  \n", r.Provenance.RunID)
	if !r.Provenance.GeneratedAt.IsZero() {
		fmt.Fprintf(w, "**generated:** %s  \n", r.Provenance.GeneratedAt.Format("2006-01-02 15:04:05 UTC"))
	}
	fmt.Fprintln(w)

	for _, jr := range r.Jobs {
		fmt.Fprintf(w, "## Job: %s\n\n", jr.JobName)
		if !hasAnyJudgments(&jr) {
			fmt.Fprintf(w, "> ⚠️ No relevance judgments — latency only.\n\n")
			writeMDLatencyTable(w, &jr)
		} else {
			writeMDAggregatedTable(w, &jr, r.Config.KValues)
			writeMDLatencyTable(w, &jr)
			writeMDSignificanceTable(w, &jr)
		}
	}
}

// newMDTable returns a go-pretty table writer that renders to GFM markdown.
// No output mirror — caller gets the rendered string via RenderMarkdown().
func newMDTable() table.Writer {
	t := table.NewWriter()
	return t
}

func writeMDAggregatedTable(w io.Writer, jr *JobReport, kValues []int) {
	writeMDMetricsTable(w, "Aggregated Results", jr.Aggregated, kValues)
	for _, cr := range jr.ByCategory {
		writeMDMetricsTable(w, fmt.Sprintf("Category: %s (%d queries)", cr.Category, cr.QueryCount), cr.Aggregated, kValues)
	}
}

func writeMDMetricsTable(w io.Writer, title string, aggregated []AggregatedEntry, kValues []int) {
	fmt.Fprintf(w, "### %s\n\n", title)

	t := newMDTable()
	hdr := make(table.Row, 0, 1+3*len(kValues)+5)
	hdr = append(hdr, "Engine")
	for _, k := range kValues {
		hdr = append(hdr, ndcgHeader(k, NDCGGain))
	}
	for _, k := range kValues {
		hdr = append(hdr, ndcgHeader(k, LinearNDCGGain))
	}
	for _, k := range kValues {
		hdr = append(hdr, fmt.Sprintf("P@%d", k))
	}
	hdr = append(hdr, "MAP", "MRR", "Bpref", fmt.Sprintf("Coverage@%d", largestK(kValues)), "Judged", "Errors")
	t.AppendHeader(hdr)

	numMetricCols := len(kValues)*3 + 4
	for _, agg := range aggregated {
		row := table.Row{agg.EngineName}
		if agg.JudgedCount > 0 {
			for _, k := range kValues {
				row = append(row, fmt.Sprintf("%.4f", agg.NDCG[k]))
			}
			for _, k := range kValues {
				row = append(row, fmtLinearNDCG(agg.NDCGLinear, k))
			}
			for _, k := range kValues {
				row = append(row, fmt.Sprintf("%.4f", agg.Precision[k]))
			}
			row = append(row,
				fmt.Sprintf("%.4f", agg.MAP),
				fmt.Sprintf("%.4f", agg.MRR),
				fmt.Sprintf("%.4f", agg.MBpref),
				fmtRecorded(agg.JudgedCoverage),
			)
		} else {
			for i := 0; i < numMetricCols; i++ {
				row = append(row, "N/A")
			}
		}
		row = append(row,
			fmt.Sprintf("%d/%d", agg.JudgedCount, agg.QueryCount),
			fmt.Sprintf("%d/%d", agg.ErrorCount, agg.QueryCount),
		)
		t.AppendRow(row)
	}
	fmt.Fprintln(w, t.RenderMarkdown())
	fmt.Fprintln(w)
}

func writeMDLatencyTable(w io.Writer, jr *JobReport) {
	fmt.Fprintf(w, "### Latency Statistics\n\n")

	t := newMDTable()
	t.AppendHeader(table.Row{
		"Engine", "Min", "p50", "p75", "p90", "p95", "p99", "Max", "Mean", "Stddev", "Samples", "Excluded",
	})
	for _, agg := range jr.Aggregated {
		s := agg.Latency
		t.AppendRow(table.Row{
			agg.EngineName,
			fmtDuration(s.Min),
			fmtDuration(s.P50()),
			fmtDuration(s.P75()),
			fmtDuration(s.P90()),
			fmtDuration(s.P95()),
			fmtDuration(s.P99()),
			fmtDuration(s.Max),
			fmtDuration(s.Mean),
			fmtDuration(s.Stddev),
			s.SampleCount,
			agg.LatencyExcludedQueries,
		})
	}
	fmt.Fprintln(w, t.RenderMarkdown())
	fmt.Fprintln(w)
}

func writeMDSignificanceTable(w io.Writer, jr *JobReport) {
	if len(jr.Significance) == 0 {
		return
	}
	fmt.Fprintf(w, "### Statistical Significance (Wilcoxon signed-rank, two-tailed, Holm-adjusted across engine pairs)\n\n")

	t := newMDTable()
	t.AppendHeader(significanceHeader)
	for _, s := range jr.Significance {
		sig := "ns"
		switch {
		case s.Untestable:
			sig = untestable
		case s.Stars != "":
			sig = s.Stars
		}
		w, p, adjusted := testCells(s)
		t.AppendRow(table.Row{s.EngineA, s.EngineB, s.Metric, fmtMeanDifference(s.MeanDifference), w, p, adjusted, sig})
	}
	fmt.Fprintln(w, t.RenderMarkdown())
	fmt.Fprintln(w)
}

// newTable returns a pre-styled go-pretty table writer.
func newTable(w io.Writer) table.Writer {
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleRounded)
	return t
}

// rightCols returns ColumnConfigs that right-align the given 1-based column numbers.
func rightCols(cols ...int) []table.ColumnConfig {
	cfgs := make([]table.ColumnConfig, len(cols))
	for i, c := range cols {
		cfgs[i] = table.ColumnConfig{Number: c, Align: text.AlignRight}
	}
	return cfgs
}

func writeAggregatedTable(w io.Writer, jr *JobReport, kValues []int) {
	writeMetricsTable(w, "Aggregated Results", jr.Aggregated, kValues)
}

func writeCategoryTables(w io.Writer, jr *JobReport, kValues []int) {
	for _, cr := range jr.ByCategory {
		title := fmt.Sprintf("Category: %s (%d queries)", cr.Category, cr.QueryCount)
		writeMetricsTable(w, title, cr.Aggregated, kValues)
	}
}

func writeMetricsTable(w io.Writer, title string, aggregated []AggregatedEntry, kValues []int) {
	fmt.Fprintf(w, "\n%s\n\n", tBold.Sprint(title))

	t := newTable(w)

	hdr := make(table.Row, 0, 1+3*len(kValues)+5)
	hdr = append(hdr, "Engine")
	for _, k := range kValues {
		hdr = append(hdr, ndcgHeader(k, NDCGGain))
	}
	for _, k := range kValues {
		hdr = append(hdr, ndcgHeader(k, LinearNDCGGain))
	}
	for _, k := range kValues {
		hdr = append(hdr, fmt.Sprintf("P@%d", k))
	}
	hdr = append(hdr, "MAP", "MRR", "Bpref", fmt.Sprintf("Coverage@%d", largestK(kValues)), "Judged", "Errors")
	t.AppendHeader(hdr)

	// Right-align all metric columns (2 … N-2); keep Judged/Errors default.
	numMetricCols := len(kValues)*3 + 4
	var rcols []int
	for i := 2; i <= 1+numMetricCols; i++ {
		rcols = append(rcols, i)
	}
	t.SetColumnConfigs(rightCols(rcols...))

	// Find per-column best values for highlighting.
	bestNDCG := make(map[int]float64, len(kValues))
	bestLinearNDCG := make(map[int]float64, len(kValues))
	bestP := make(map[int]float64, len(kValues))
	bestMAP, bestMRR, bestBpref := math.Inf(-1), math.Inf(-1), math.Inf(-1)
	for _, k := range kValues {
		bestNDCG[k] = math.Inf(-1)
		bestLinearNDCG[k] = math.Inf(-1)
		bestP[k] = math.Inf(-1)
	}
	for _, agg := range aggregated {
		if agg.JudgedCount == 0 {
			continue
		}
		for _, k := range kValues {
			if v := agg.NDCG[k]; v > bestNDCG[k] {
				bestNDCG[k] = v
			}
			if v, ok := agg.NDCGLinear[k]; ok && v > bestLinearNDCG[k] {
				bestLinearNDCG[k] = v
			}
			if v := agg.Precision[k]; v > bestP[k] {
				bestP[k] = v
			}
		}
		if agg.MAP > bestMAP {
			bestMAP = agg.MAP
		}
		if agg.MRR > bestMRR {
			bestMRR = agg.MRR
		}
		if agg.MBpref > bestBpref {
			bestBpref = agg.MBpref
		}
	}

	for _, agg := range aggregated {
		row := table.Row{agg.EngineName}
		if agg.JudgedCount > 0 {
			for _, k := range kValues {
				row = append(row, fmtBest(agg.NDCG[k], bestNDCG[k]))
			}
			for _, k := range kValues {
				row = append(row, fmtBestLinearNDCG(agg.NDCGLinear, k, bestLinearNDCG[k]))
			}
			for _, k := range kValues {
				row = append(row, fmtBest(agg.Precision[k], bestP[k]))
			}
			row = append(row,
				fmtBest(agg.MAP, bestMAP),
				fmtBest(agg.MRR, bestMRR),
				fmtBest(agg.MBpref, bestBpref),
				fmtRecorded(agg.JudgedCoverage),
			)
		} else {
			for i := 0; i < numMetricCols; i++ {
				row = append(row, tDim.Sprint("N/A"))
			}
		}
		row = append(row,
			fmt.Sprintf("%d/%d", agg.JudgedCount, agg.QueryCount),
			fmt.Sprintf("%d/%d", agg.ErrorCount, agg.QueryCount),
		)
		t.AppendRow(row)
	}

	t.Render()
	fmt.Fprintln(w)
}

// fmtBest formats a metric value and highlights it green when it equals best.
// Ties are both highlighted.
func fmtBest(v, best float64) string {
	s := fmt.Sprintf("%.4f", v)
	if v == best {
		return tGreen.Sprint(s)
	}
	return s
}

func writeLatencyTable(w io.Writer, jr *JobReport) {
	fmt.Fprintf(w, "%s\n\n", tBold.Sprint("Latency Statistics"))

	// Fastest p50 gets highlighted.
	bestP50 := time.Duration(math.MaxInt64)
	for _, agg := range jr.Aggregated {
		if p := agg.Latency.P50(); p > 0 && p < bestP50 {
			bestP50 = p
		}
	}

	t := newTable(w)
	t.AppendHeader(table.Row{
		"Engine", "Min", "p50", "p75", "p90", "p95", "p99", "Max", "Mean", "Stddev", "Samples", "Excluded",
	})
	t.SetColumnConfigs(rightCols(2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12))

	for _, agg := range jr.Aggregated {
		s := agg.Latency
		p50 := agg.Latency.P50()
		p50Str := fmtDuration(p50)
		if p50 == bestP50 {
			p50Str = tGreen.Sprint(p50Str)
		}
		t.AppendRow(table.Row{
			agg.EngineName,
			fmtDuration(s.Min),
			p50Str,
			fmtDuration(s.P75()),
			fmtDuration(s.P90()),
			fmtDuration(s.P95()),
			fmtDuration(s.P99()),
			fmtDuration(s.Max),
			fmtDuration(s.Mean),
			fmtDuration(s.Stddev),
			s.SampleCount,
			agg.LatencyExcludedQueries,
		})
	}

	t.Render()
	fmt.Fprintln(w)
}

func writeSignificanceTable(w io.Writer, jr *JobReport) {
	if len(jr.Significance) == 0 {
		return
	}
	fmt.Fprintf(w, "%s\n\n",
		tBold.Sprint("Statistical Significance (Wilcoxon signed-rank, two-tailed, Holm-adjusted across engine pairs · * p<0.05 · ** p<0.01)"))

	t := newTable(w)
	t.AppendHeader(significanceHeader)
	t.SetColumnConfigs(rightCols(4, 5, 6, 7))

	for _, s := range jr.Significance {
		sig := colorSig(s.Stars)
		if s.Untestable {
			sig = tDim.Sprint(untestable)
		}
		w, p, adjusted := testCells(s)
		t.AppendRow(table.Row{s.EngineA, s.EngineB, s.Metric, fmtMeanDifference(s.MeanDifference), w, p, adjusted, sig})
	}

	t.Render()
	fmt.Fprintln(w)
}

var significanceHeader = table.Row{"Engine A", "Engine B", "Metric", "Δ A−B [95% CI]", "W", "p-value", "p (Holm)", "Sig"}

// untestable marks a pair with fewer than 4 non-zero differences, which
// Wilcoxon cannot test.
const untestable = "untestable"

func testCells(s PairwiseSignificance) (w, p, adjusted string) {
	if s.Untestable {
		return "—", "—", "—"
	}
	return fmt.Sprintf("%.1f", s.W), fmt.Sprintf("%.4f", s.P), fmtRecorded(s.AdjustedP)
}

// fmtMeanDifference shows a dash for reports written before the difference was
// recorded, which would otherwise read as no difference.
func fmtMeanDifference(d *metrics.Interval) string {
	if d == nil {
		return "—"
	}
	return fmt.Sprintf("%+.4f [%+.4f, %+.4f]", d.Mean, d.Low, d.High)
}

func colorSig(stars string) string {
	switch stars {
	case "**":
		return tGreen.Sprint("**")
	case "*":
		return tYellow.Sprint("*")
	default:
		return tDim.Sprint("ns")
	}
}

func writePerQueryTable(w io.Writer, jr *JobReport, kValues []int) {
	fmt.Fprintf(w, "%s\n\n", tBold.Sprint("Per-Query Results"))

	k := largestK(kValues)

	t := newTable(w)
	t.AppendHeader(table.Row{
		"Query", "Engine",
		ndcgHeader(k, NDCGGain), fmt.Sprintf("P@%d", k),
		"AP", "RR", "Bpref",
		"Ret", "Corpus", "p50", "p95", "Status",
	})
	t.SetColumnConfigs(rightCols(3, 4, 5, 6, 7, 8, 9, 10))

	for _, e := range jr.PerQuery {
		var status string
		if e.Error != "" {
			status = tRed.Sprint("ERR")
		} else {
			status = tGreen.Sprint("OK")
		}

		apStr, rrStr, bprefStr := tDim.Sprint("—"), tDim.Sprint("—"), tDim.Sprint("—")
		if e.Judged {
			apStr = fmt.Sprintf("%.4f", e.AP)
			rrStr = fmt.Sprintf("%.4f", e.RR)
			bprefStr = fmt.Sprintf("%.4f", e.Bpref)
		}

		t.AppendRow(table.Row{
			e.QueryID, e.EngineName,
			fmtScore(e.NDCG, k), fmtScore(e.Precision, k),
			apStr, rrStr, bprefStr,
			e.ReturnedCount, fmtCorpusMatches(e.CorpusMatches),
			fmtDuration(e.Latency.P50()), fmtDuration(e.Latency.P95()),
			status,
		})
	}

	t.Render()
	fmt.Fprintln(w)
}

func fmtCorpusMatches(v *int64) string {
	if v == nil {
		return tDim.Sprint("—")
	}
	return fmt.Sprintf("%d", *v)
}

// Every NDCG header names its gain, because the two can order engines
// differently. NDCGGain is the bench's own; LinearNDCGGain is trec_eval's
// ndcg_cut, which ignores the relevance threshold.
const (
	NDCGGain       = "2^rel-1"
	LinearNDCGGain = "rel"
)

func ndcgHeader(k int, gain string) string {
	return fmt.Sprintf("NDCG@%d (gain %s)", k, gain)
}

// fmtLinearNDCG shows a dash for reports written before linear NDCG was
// recorded, which would otherwise read as 0.
func fmtLinearNDCG(scores map[int]float64, k int) string {
	if scores == nil {
		return "—"
	}
	return fmt.Sprintf("%.4f", scores[k])
}

func fmtBestLinearNDCG(scores map[int]float64, k int, best float64) string {
	if scores == nil {
		return tDim.Sprint("—")
	}
	return fmtBest(scores[k], best)
}

func largestK(kValues []int) int {
	if len(kValues) == 0 {
		return 0
	}
	return slices.Max(kValues)
}

func fmtScore(scores map[int]float64, k int) string {
	if scores == nil {
		return "N/A"
	}
	return fmt.Sprintf("%.4f", scores[k])
}

func fmtDuration(d time.Duration) string {
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
