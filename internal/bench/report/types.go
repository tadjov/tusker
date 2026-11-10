package report

import (
	"runtime"
	"time"

	"github.com/DjordjeVuckovic/tusker/internal/bench/meta"
	"github.com/DjordjeVuckovic/tusker/internal/bench/metrics"
	"github.com/DjordjeVuckovic/tusker/internal/bench/runner"
	"github.com/google/uuid"
)

// Report is the root artifact produced by bench run. It has exactly two
// identity blocks:
//
//   - Provenance (meta.Meta) — who/what/when produced this report and which
//     source artifacts it consumed (spec, suite, pool, judgments).
//   - Environment (RunEnvironment) — runtime context: engines tested, corpus
//     shape, and host platform.
//
// There is no legacy "meta" block — Version and Timestamp lived there but are
// already in Provenance.Tool and Provenance.GeneratedAt.
type Report struct {
	SchemaVersion int            `json:"schema_version"`
	Provenance    meta.Meta      `json:"provenance"`
	Environment   RunEnvironment `json:"environment"`
	Jobs          []JobReport    `json:"jobs"`
	Config        ReportConfig   `json:"config"`
	Settings      RunSettings    `json:"settings"`
}

// RunEnvironment captures the runtime context of a bench run — engines tested,
// corpus shape, and host platform. Distinct from Provenance (identity/sources)
// and from Jobs (the measured outcomes).
type RunEnvironment struct {
	Engines  map[string]EngineInfo `json:"engines"`
	Corpus   CorpusInfo            `json:"corpus,omitempty"`
	Platform PlatformInfo          `json:"platform"`
}

type EngineInfo struct {
	Type       string `json:"type"`
	Connection string `json:"connection"`
	Version    string `json:"version,omitempty"`
	// Index is nil in reports written before index provenance was recorded.
	Index *IndexProvenance `json:"index,omitempty"`
}

type CorpusInfo struct {
	Name      string           `json:"name,omitempty"`
	DocCount  int64            `json:"doc_count,omitempty"`
	IndexName string           `json:"index_name,omitempty"`
	PerEngine map[string]int64 `json:"per_engine,omitempty"`
}

type PlatformInfo struct {
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	NumCPU    int    `json:"num_cpu"`
}

func NewPlatformInfo() PlatformInfo {
	return PlatformInfo{
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		NumCPU:    runtime.NumCPU(),
	}
}

type JobReport struct {
	JobName    string
	Aggregated []AggregatedEntry
	// Empty when the suite tags fewer than two categories.
	ByCategory   []CategoryReport
	PerQuery     []Entry
	Significance []PairwiseSignificance
}

type CategoryReport struct {
	Category   string
	QueryCount int
	Aggregated []AggregatedEntry
}

// PairwiseSignificance compares two engines on one metric across all judged
// queries in this job: a Wilcoxon signed-rank test and the mean difference.
type PairwiseSignificance struct {
	EngineA string  // reference engine (listed first in spec jobs[].engines)
	EngineB string  // comparison engine
	Metric  string  // e.g. "NDCG@10", "MAP", "MRR"
	W       float64 // Wilcoxon W statistic
	P       float64 // two-tailed p-value, unadjusted
	// Untestable marks a pair with fewer than 4 non-zero differences. It keeps
	// P = 1 so the metric's Holm family still counts it.
	Untestable bool
	// AdjustedP is Holm-corrected across the job's engine pairs on this
	// metric; nil in reports written before the correction.
	AdjustedP *float64
	Stars     string // from AdjustedP: "**" p<0.01 | "*" p<0.05 | "" not significant
	// MeanDifference is the mean of A − B with its 95% bootstrap interval;
	// nil in reports written before it was recorded.
	MeanDifference *metrics.Interval
}

type ReportConfig struct {
	KValues            []int
	RelevanceThreshold int
}

type Entry struct {
	QueryID       string
	Category      string
	JobName       string
	EngineName    string
	Judged        bool
	NDCG          map[int]float64
	NDCGLinear    map[int]float64 `json:",omitempty"`
	Precision     map[int]float64
	Recall        map[int]float64
	F1            map[int]float64
	AP            float64
	RR            float64
	Bpref         float64
	ReturnedCount int
	CorpusMatches *int64
	Latency       LatencyStats
	Error         string
	// Ranking is what the engine returned, best first; nil in reports written
	// before rankings were recorded.
	Ranking []RankedDoc `json:",omitempty"`
}

// RankedDoc is one retrieved document. Engines report no scores today, so
// Score is derived from rank: it strictly decreases down the list, which is
// all trec_eval reads from it.
type RankedDoc struct {
	DocID uuid.UUID `json:"doc_id"`
	Score float64   `json:"score"`
}

type AggregatedEntry struct {
	EngineName string
	NDCG       map[int]float64
	NDCGStddev map[int]float64 // sample stddev of per-query NDCG — measures consistency
	// NDCGLinear is nil in reports written before it was recorded.
	NDCGLinear map[int]float64 `json:",omitempty"`
	Precision  map[int]float64
	Recall     map[int]float64
	F1         map[int]float64
	MAP        float64
	MRR        float64
	MBpref     float64
	// Latency covers only the queries every engine in the job answered;
	// LatencyExcludedQueries counts the rest.
	Latency                LatencyStats
	LatencyExcludedQueries int
	QueryCount             int
	// JudgedCount is the size of the job's judged query set, the denominator
	// of every quality mean. A query this engine failed or had no block for
	// scores 0.
	JudgedCount int
	// ErrorCount includes queries this engine had no block for.
	ErrorCount int
	// JudgedCoverage is the share of retrieved docs within the largest K, over
	// the judged queries, that carry a grade. Ungraded docs score as
	// non-relevant, so a low value means the metrics above undercount. nil when
	// nothing was retrieved or the report predates it.
	JudgedCoverage *float64
}

type LatencyStats struct {
	Min         time.Duration         `json:"min"`
	Max         time.Duration         `json:"max"`
	Mean        time.Duration         `json:"mean"`
	Median      time.Duration         `json:"median"`
	Stddev      time.Duration         `json:"stddev"`
	Percentiles map[int]time.Duration `json:"percentiles"`
	SampleCount int                   `json:"sample_count"`
}

func fromRunnerLatencyStats(s runner.LatencyStats) LatencyStats {
	return LatencyStats{
		Min:         s.Min,
		Max:         s.Max,
		Mean:        s.Mean,
		Median:      s.Median,
		Stddev:      s.Stddev,
		Percentiles: s.Percentiles,
		SampleCount: s.SampleCount,
	}
}

func (s LatencyStats) P50() time.Duration { return s.Percentiles[50] }
func (s LatencyStats) P75() time.Duration { return s.Percentiles[75] }
func (s LatencyStats) P90() time.Duration { return s.Percentiles[90] }
func (s LatencyStats) P95() time.Duration { return s.Percentiles[95] }
func (s LatencyStats) P99() time.Duration { return s.Percentiles[99] }

// RunSettings are the knobs a run was measured with. K is Config.KValues.
type RunSettings struct {
	Warmup     int `json:"warmup"`
	Iterations int `json:"iterations"`
	// QueryTimeout is the deadline each execution ran under.
	QueryTimeout time.Duration `json:"query_timeout"`
	// JobFilter is the --jobs selection; empty when the run took every job.
	JobFilter []string `json:"job_filter,omitempty"`
	// LatencyOnly marks a run made without judgments: its quality metrics are
	// absent, not zero.
	LatencyOnly bool `json:"latency_only"`
	// EngineParams are the template defaults each engine declares in the spec.
	EngineParams map[string]map[string]any `json:"engine_params,omitempty"`
	// Incomplete lists why the run cannot stand as the track's result. bench
	// run never points latest.json at a report that has any.
	Incomplete []string `json:"incomplete,omitempty"`
}
