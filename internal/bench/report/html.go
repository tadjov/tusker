package report

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed report.gohtml
var htmlTemplate string

// WriteHTML renders r as a self-contained HTML file at path, creating parent
// directories as needed. The file contains inline CSS, inline SVG charts, and
// ~60 lines of vanilla JS for sortable tables and per-query filtering — no
// external dependencies, opens offline.
func WriteHTML(r *Report, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create report dir: %w", err)
	}
	buf, err := RenderHTML(r)
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0644)
}

// RenderHTML returns the fully rendered HTML bytes for a report.
func RenderHTML(r *Report) ([]byte, error) {
	vm := buildViewModel(r)
	tmpl, err := template.New("report").Funcs(htmlFuncs()).Parse(htmlTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse html template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vm); err != nil {
		return nil, fmt.Errorf("render html: %w", err)
	}
	return buf.Bytes(), nil
}

// ─── git-relative path helper ────────────────────────────────────────────────

// repoRelPath makes path relative to the nearest .git root it can find by
// walking up. Falls back to the path's basename if no git root is found.
// This keeps source paths readable in shared reports.
func repoRelPath(path string) string {
	if path == "" {
		return ""
	}
	root := findGitRoot(filepath.Dir(path))
	if root == "" {
		return filepath.Base(path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

func findGitRoot(start string) string {
	dir := start
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// ─── view model ─────────────────────────────────────────────────────────────

type htmlReport struct {
	NDCGGain       string
	LinearNDCGGain string
	Title          string
	RunID          string
	Tool           string
	Generated      string
	SpecID         string
	Sources        *htmlSources
	Jobs           []htmlJob
}

type htmlSources struct {
	Spec, Suites, Pool, Judgments string
}

type htmlLegendItem struct {
	Name  string
	Color string
}

type htmlJob struct {
	Name         string
	Aggregated   []htmlAggRow
	Latency      []htmlLatRow
	Significance []htmlSigRow
	PerQuery     []htmlQueryRow
	NDCGChart    template.HTML
	LatencyChart template.HTML
	EngineLegend []htmlLegendItem
	KValues      []int
	CoverageK    int
	FilterID     string // unique ID for per-query filter input
}

type htmlAggRow struct {
	Engine     string
	NDCG       []htmlCell
	NDCGLinear []string
	MAP        htmlCell
	MRR        htmlCell
	Bpref      htmlCell
	Coverage   string
	Judged     string
	Errors     string
}

type htmlCell struct {
	Val    string
	Stddev string
}

type htmlLatRow struct {
	Engine                                          string
	Min, P50, P75, P90, P95, P99, Max, Mean, Stddev string
	Samples                                         int
	ExcludedQueries                                 int
}

type htmlSigRow struct {
	EngineA, EngineB, Metric string
	W, P, AdjustedP          string
	Stars                    string
	NS                       bool
}

type htmlQueryRow struct {
	Query, Engine, NDCG, Precision, AP, RR, Bpref string
	ReturnedCount                                 int
	CorpusMatches                                 string
	P50, P95                                      string
	Status                                        string
	IsErr                                         bool
}

func htmlCorpusMatches(v *int64) string {
	if v == nil {
		return "—"
	}
	return strconv.FormatInt(*v, 10)
}

func buildViewModel(r *Report) htmlReport {
	title := r.Provenance.SpecID
	if title == "" {
		title = "Benchmark Report"
	}
	vm := htmlReport{
		NDCGGain:       NDCGGain,
		LinearNDCGGain: LinearNDCGGain,
		Title:          title,
		RunID:          r.Provenance.RunID,
		Tool:           r.Provenance.Tool,
		Generated:      r.Provenance.GeneratedAt.Format("2006-01-02 15:04:05 UTC"),
		SpecID:         r.Provenance.SpecID,
	}
	if s := r.Provenance.Sources; s != nil {
		vm.Sources = &htmlSources{
			Spec:      repoRelPath(s.Spec),
			Suites:    repoRelPaths(s.Suites),
			Pool:      repoRelPath(s.Pool),
			Judgments: repoRelPath(s.Judgments),
		}
	}

	kVals := r.Config.KValues

	for ji, jr := range r.Jobs {
		job := htmlJob{
			Name:      jr.JobName,
			KValues:   kVals,
			CoverageK: largestK(kVals),
			FilterID:  fmt.Sprintf("pq-filter-%d", ji),
		}

		for i, agg := range jr.Aggregated {
			color := engineColors[i%len(engineColors)]
			job.EngineLegend = append(job.EngineLegend, htmlLegendItem{Name: agg.EngineName, Color: color})

			row := htmlAggRow{Engine: agg.EngineName}
			for _, k := range kVals {
				c := htmlCell{Val: fmt.Sprintf("%.4f", agg.NDCG[k])}
				if sd := agg.NDCGStddev[k]; sd > 0 {
					c.Stddev = fmt.Sprintf("%.4f", sd)
				}
				row.NDCG = append(row.NDCG, c)
			}
			for _, k := range kVals {
				row.NDCGLinear = append(row.NDCGLinear, fmtLinearNDCG(agg.NDCGLinear, k))
			}
			row.MAP = htmlCell{Val: fmt.Sprintf("%.4f", agg.MAP)}
			row.MRR = htmlCell{Val: fmt.Sprintf("%.4f", agg.MRR)}
			row.Bpref = htmlCell{Val: fmt.Sprintf("%.4f", agg.MBpref)}
			row.Coverage = fmtRecorded(agg.JudgedCoverage)
			row.Judged = fmt.Sprintf("%d/%d", agg.JudgedCount, agg.QueryCount)
			row.Errors = fmt.Sprintf("%d/%d", agg.ErrorCount, agg.QueryCount)
			job.Aggregated = append(job.Aggregated, row)
		}

		for _, agg := range jr.Aggregated {
			s := agg.Latency
			job.Latency = append(job.Latency, htmlLatRow{
				Engine:          agg.EngineName,
				Min:             fmtDuration(s.Min),
				P50:             fmtDuration(s.P50()),
				P75:             fmtDuration(s.P75()),
				P90:             fmtDuration(s.P90()),
				P95:             fmtDuration(s.P95()),
				P99:             fmtDuration(s.P99()),
				Max:             fmtDuration(s.Max),
				Mean:            fmtDuration(s.Mean),
				Stddev:          fmtDuration(s.Stddev),
				Samples:         s.SampleCount,
				ExcludedQueries: agg.LatencyExcludedQueries,
			})
		}

		for _, sig := range jr.Significance {
			job.Significance = append(job.Significance, htmlSigRow{
				EngineA: sig.EngineA, EngineB: sig.EngineB, Metric: sig.Metric,
				W: fmt.Sprintf("%.1f", sig.W), P: fmt.Sprintf("%.4f", sig.P), AdjustedP: fmtRecorded(sig.AdjustedP),
				Stars: sig.Stars, NS: sig.Stars == "",
			})
		}

		k := primaryK(kVals)
		for _, e := range jr.PerQuery {
			status, isErr := "OK", false
			if e.Error != "" {
				status, isErr = "ERR", true
			}
			ap, rr, bp := "—", "—", "—"
			if e.Judged {
				ap = fmt.Sprintf("%.4f", e.AP)
				rr = fmt.Sprintf("%.4f", e.RR)
				bp = fmt.Sprintf("%.4f", e.Bpref)
			}
			job.PerQuery = append(job.PerQuery, htmlQueryRow{
				Query: e.QueryID, Engine: e.EngineName,
				NDCG: fmtScore(e.NDCG, k), Precision: fmtScore(e.Precision, k),
				AP: ap, RR: rr, Bpref: bp,
				ReturnedCount: e.ReturnedCount, CorpusMatches: htmlCorpusMatches(e.CorpusMatches),
				P50: fmtDuration(e.Latency.P50()), P95: fmtDuration(e.Latency.P95()),
				Status: status, IsErr: isErr,
			})
		}

		job.NDCGChart = buildNDCGChart(jr.Aggregated, kVals)
		job.LatencyChart = buildLatencyChart(jr.Aggregated)
		vm.Jobs = append(vm.Jobs, job)
	}
	return vm
}

// ─── SVG charts ──────────────────────────────────────────────────────────────

var engineColors = []string{
	"#4C8EDA", "#E05C5C", "#52C878", "#F5A623", "#9B59B6", "#1ABC9C",
}

// buildNDCGChart renders a grouped bar chart of NDCG@K scores per engine.
// The engine legend is NOT included in the SVG; it is rendered in HTML below
// the chart using job.EngineLegend — this avoids text overflow for long names.
func buildNDCGChart(aggregated []AggregatedEntry, kVals []int) template.HTML {
	if len(aggregated) == 0 || len(kVals) == 0 {
		return ""
	}
	const (
		svgW      = 560
		svgH      = 190
		padLeft   = 46
		padRight  = 16
		padTop    = 20
		padBottom = 30 // only K-value labels, no legend
	)
	plotW := svgW - padLeft - padRight
	plotH := svgH - padTop - padBottom

	nGroups := len(kVals)
	nEngines := len(aggregated)
	groupW := float64(plotW) / float64(nGroups)
	barW := groupW / float64(nEngines+1)

	var sb strings.Builder
	// viewBox makes the chart responsive; width=100% fills the container.
	sb.WriteString(fmt.Sprintf(
		`<svg viewBox="0 0 %d %d" width="100%%" xmlns="http://www.w3.org/2000/svg">`,
		svgW, svgH))

	// Gridlines + Y-axis labels.
	for _, tick := range []float64{0, 0.25, 0.5, 0.75, 1.0} {
		y := padTop + plotH - int(tick*float64(plotH))
		sb.WriteString(fmt.Sprintf(
			`<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#e5e7eb" stroke-width="1"/>`,
			padLeft, y, svgW-padRight, y))
		sb.WriteString(fmt.Sprintf(
			`<text x="%d" y="%d" text-anchor="end" font-size="10" fill="#9ca3af">%.2f</text>`,
			padLeft-4, y+4, tick))
	}

	// Bars + value labels.
	for gi, k := range kVals {
		groupX := padLeft + int(float64(gi)*groupW)
		for ei, agg := range aggregated {
			val := math.Min(agg.NDCG[k], 1.0)
			barH := int(val * float64(plotH))
			x := groupX + int(float64(ei)*barW) + int(barW*0.15)
			y := padTop + plotH - barH
			color := engineColors[ei%len(engineColors)]
			sb.WriteString(fmt.Sprintf(
				`<rect x="%d" y="%d" width="%d" height="%d" fill="%s" rx="2"/>`,
				x, y, max(int(barW*0.75), 1), barH, color))
			if barH > 14 {
				sb.WriteString(fmt.Sprintf(
					`<text x="%d" y="%d" text-anchor="middle" font-size="9" fill="#fff">%.2f</text>`,
					x+max(int(barW*0.75), 1)/2, y+12, val))
			}
		}
		// K-value label below group.
		labelX := groupX + int(groupW/2)
		sb.WriteString(fmt.Sprintf(
			`<text x="%d" y="%d" text-anchor="middle" font-size="12" fill="#374151">@%d</text>`,
			labelX, padTop+plotH+18, k))
	}

	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}

// buildLatencyChart renders a horizontal bar chart of p50 latency per engine
// on a logarithmic scale, making order-of-magnitude differences visible.
func buildLatencyChart(aggregated []AggregatedEntry) template.HTML {
	if len(aggregated) == 0 {
		return ""
	}

	type latEntry struct {
		name   string
		us     float64 // p50 in microseconds
		valStr string
		color  string
	}
	var entries []latEntry
	for i, agg := range aggregated {
		us := float64(agg.Latency.P50().Microseconds())
		if us < 1 {
			us = 1
		}
		entries = append(entries, latEntry{
			name:   agg.EngineName,
			us:     us,
			valStr: fmtDuration(agg.Latency.P50()),
			color:  engineColors[i%len(engineColors)],
		})
	}

	const (
		svgW   = 560
		barH   = 26
		barGap = 10
		padL   = 120
		padR   = 80
		padV   = 10
	)
	plotW := svgW - padL - padR
	n := len(entries)
	svgH := padV*2 + n*(barH+barGap) - barGap

	// Log-scale range.
	minLog := math.Log10(entries[0].us)
	maxLog := minLog
	for _, e := range entries {
		l := math.Log10(e.us)
		if l < minLog {
			minLog = l
		}
		if l > maxLog {
			maxLog = l
		}
	}
	logRange := maxLog - minLog
	if logRange < 0.3 {
		logRange = 0.3 // minimum range so bars differ visually
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(
		`<svg viewBox="0 0 %d %d" width="100%%" xmlns="http://www.w3.org/2000/svg">`,
		svgW, svgH))

	for i, e := range entries {
		y := padV + i*(barH+barGap)
		cy := y + barH/2

		frac := (math.Log10(e.us) - minLog) / logRange
		bw := int(frac*float64(plotW)) + 6 // +6 minimum so fastest engine is visible

		// Engine name.
		sb.WriteString(fmt.Sprintf(
			`<text x="%d" y="%d" text-anchor="end" font-size="12" fill="#374151" dominant-baseline="middle">%s</text>`,
			padL-8, cy, template.HTMLEscapeString(e.name)))
		// Bar.
		sb.WriteString(fmt.Sprintf(
			`<rect x="%d" y="%d" width="%d" height="%d" fill="%s" rx="3" opacity="0.85"/>`,
			padL, y, bw, barH, e.color))
		// Value label.
		sb.WriteString(fmt.Sprintf(
			`<text x="%d" y="%d" font-size="11" fill="#374151" dominant-baseline="middle">%s</text>`,
			padL+bw+6, cy, e.valStr))
	}

	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}

// ─── template helpers ────────────────────────────────────────────────────────

func htmlFuncs() template.FuncMap {
	return template.FuncMap{
		"fmtTime": func(t time.Time) string { return t.Format("2006-01-02 15:04:05 UTC") },
	}
}
