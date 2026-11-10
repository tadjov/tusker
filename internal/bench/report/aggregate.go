package report

import (
	"fmt"
	"hash/fnv"
	"math"
	"net/url"
	"strings"

	"github.com/DjordjeVuckovic/tusker/internal/bench/meta"
	"github.com/DjordjeVuckovic/tusker/internal/bench/metrics"
	"github.com/DjordjeVuckovic/tusker/internal/bench/runner"
	"github.com/DjordjeVuckovic/tusker/internal/bench/spec"
	"github.com/DjordjeVuckovic/tusker/internal/bench/version"
)

type GenerateOptions struct {
	Spec    *spec.BenchSpec
	Corpus  CorpusInfo
	Indexes map[string]IndexProvenance
}

func Generate(br *runner.BenchmarkResult, opts *GenerateOptions) *Report {
	r := &Report{
		SchemaVersion: version.SchemaVersion,
		Provenance:    meta.New("run"),
		Environment: RunEnvironment{
			Engines:  make(map[string]EngineInfo),
			Platform: NewPlatformInfo(),
		},
		Config: ReportConfig{
			KValues:            br.Config.KValues,
			RelevanceThreshold: br.Config.RelevanceThreshold,
		},
	}

	if opts != nil {
		if opts.Spec != nil {
			for name, eng := range opts.Spec.Engines {
				info := EngineInfo{
					Type:       string(eng.Type),
					Connection: connectionEndpoint(eng.Connection),
				}
				if provenance, ok := opts.Indexes[name]; ok {
					info.Version = provenance.Version()
					info.Index = &provenance
				}
				r.Environment.Engines[name] = info
			}
		}
		r.Environment.Corpus = opts.Corpus
	}

	for _, jr := range br.Jobs {
		r.Jobs = append(r.Jobs, generateJobReport(newJobScope(jr, br.Config)))
	}

	return r
}

// connectionEndpoint keeps only where an engine lives, never how it
// authenticates: a URL loses its user info and query, a key=value DSN keeps
// only host, port and dbname.
func connectionEndpoint(conn string) string {
	if u, err := url.Parse(conn); err == nil && u.Scheme != "" && u.Host != "" {
		return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
	}
	var kept []string
	for _, field := range strings.Fields(conn) {
		key, _, ok := strings.Cut(field, "=")
		if ok && (key == "host" || key == "port" || key == "dbname") {
			kept = append(kept, field)
		}
	}
	return strings.Join(kept, " ")
}

// jobScope is what every aggregate of one job is computed against. Quality
// metrics average over the judged queries, scoring a failed or missing
// (query, engine) cell as 0, as trec_eval -c does.
type jobScope struct {
	job     *runner.JobResult
	kValues []int
	judged  map[string]bool
	// grades[queryID][docID] is empty for suite-embedded judgments, which
	// never reach the runner config.
	grades map[string]map[string]int
}

func newJobScope(jr *runner.JobResult, cfg runner.Config) jobScope {
	judged := make(map[string]bool, len(jr.QueryOrder))
	for _, qID := range jr.QueryOrder {
		if len(cfg.Judgments[qID]) > 0 {
			judged[qID] = true
			continue
		}
		// Suite-embedded grades never reach cfg.Judgments; a scored cell is
		// the only trace of them.
		for _, qr := range jr.Results[qID] {
			if qr.Scores.Judged {
				judged[qID] = true
			}
		}
	}
	return jobScope{job: jr, kValues: cfg.KValues, judged: judged, grades: cfg.Judgments}
}

// scoresOf returns zero scores for a cell that failed or was never run.
func (s jobScope) scoresOf(qID, engName string) metrics.ScoreSet {
	qr, ok := s.job.Results[qID][engName]
	if !ok || qr.Error != nil {
		return metrics.ScoreSet{}
	}
	return qr.Scores
}

func (s jobScope) answeredByEveryEngine(qID string) bool {
	for _, engName := range s.job.EngineNames {
		qr, ok := s.job.Results[qID][engName]
		if !ok || qr.Error != nil {
			return false
		}
	}
	return true
}

// gradedInTopK counts the cell's retrieved docs within the largest K, and how
// many of them carry a grade. A query graded only in the suite file counts
// nothing, since its grades are not visible here.
func (s jobScope) gradedInTopK(qID, engName string) (graded, retrieved int) {
	qr, ok := s.job.Results[qID][engName]
	if !ok || qr.Error != nil || len(s.grades[qID]) == 0 {
		return 0, 0
	}
	top := qr.RankedDocIDs[:min(largestK(s.kValues), len(qr.RankedDocIDs))]
	for _, docID := range top {
		if _, ok := s.grades[qID][docID.String()]; ok {
			graded++
		}
	}
	return graded, len(top)
}

func generateJobReport(scope jobScope) JobReport {
	jr := scope.job
	report := JobReport{JobName: jr.JobName}

	for _, qID := range jr.QueryOrder {
		engineResults := jr.Results[qID]
		for _, engName := range jr.EngineNames {
			qr, ok := engineResults[engName]
			if !ok {
				continue
			}
			entry := Entry{
				QueryID:       qr.QueryID,
				Category:      qr.Category,
				JobName:       jr.JobName,
				EngineName:    qr.EngineName,
				Judged:        qr.Scores.Judged,
				NDCG:          qr.Scores.NDCG,
				NDCGLinear:    qr.Scores.NDCGLinear,
				Precision:     qr.Scores.Precision,
				Recall:        qr.Scores.Recall,
				F1:            qr.Scores.F1,
				AP:            qr.Scores.AP,
				RR:            qr.Scores.RR,
				Bpref:         qr.Scores.Bpref,
				ReturnedCount: qr.ReturnedCount,
				CorpusMatches: qr.CorpusMatches,
				Latency:       fromRunnerLatencyStats(qr.Latency),
				Ranking:       rankDerivedScores(qr.RankedDocIDs),
			}
			if qr.Error != nil {
				entry.Error = qr.Error.Error()
				entry.Judged = scope.judged[qID]
			}
			report.PerQuery = append(report.PerQuery, entry)
		}
	}

	report.Aggregated = aggregate(scope, jr.QueryOrder)
	report.ByCategory = aggregateByCategory(scope)
	report.Significance = computeSignificance(scope)
	return report
}

// Categories keep the order they first appear in the suite.
func aggregateByCategory(scope jobScope) []CategoryReport {
	jr := scope.job
	var order []string
	members := make(map[string][]string)

	for _, qID := range jr.QueryOrder {
		cat := categoryOf(jr, qID)
		if cat == "" {
			continue
		}
		if _, seen := members[cat]; !seen {
			order = append(order, cat)
		}
		members[cat] = append(members[cat], qID)
	}
	if len(order) < 2 {
		return nil
	}

	reports := make([]CategoryReport, 0, len(order))
	for _, cat := range order {
		reports = append(reports, CategoryReport{
			Category:   cat,
			QueryCount: len(members[cat]),
			Aggregated: aggregate(scope, members[cat]),
		})
	}
	return reports
}

func categoryOf(jr *runner.JobResult, qID string) string {
	for _, qr := range jr.Results[qID] {
		if qr.Category != "" {
			return qr.Category
		}
	}
	return ""
}

func aggregate(scope jobScope, queryIDs []string) []AggregatedEntry {
	kValues := scope.kValues
	entries := make([]AggregatedEntry, 0, len(scope.job.EngineNames))

	for _, engName := range scope.job.EngineNames {
		agg := AggregatedEntry{
			EngineName: engName,
			NDCG:       make(map[int]float64, len(kValues)),
			NDCGStddev: make(map[int]float64, len(kValues)),
			NDCGLinear: make(map[int]float64, len(kValues)),
			Precision:  make(map[int]float64, len(kValues)),
			Recall:     make(map[int]float64, len(kValues)),
			F1:         make(map[int]float64, len(kValues)),
			QueryCount: len(queryIDs),
		}

		ndcgSamples := make(map[int][]float64, len(kValues))
		var allStats []runner.LatencyStats
		var gradedTopK, retrievedTopK int

		for _, qID := range queryIDs {
			qr, ok := scope.job.Results[qID][engName]
			if !ok || qr.Error != nil {
				agg.ErrorCount++
			}

			if scope.answeredByEveryEngine(qID) {
				allStats = append(allStats, qr.Latency)
			} else {
				agg.LatencyExcludedQueries++
			}

			if !scope.judged[qID] {
				continue
			}

			graded, retrieved := scope.gradedInTopK(qID, engName)
			gradedTopK += graded
			retrievedTopK += retrieved

			scores := scope.scoresOf(qID, engName)
			agg.JudgedCount++
			agg.MAP += scores.AP
			agg.MRR += scores.RR
			agg.MBpref += scores.Bpref

			for _, k := range kValues {
				v := scores.NDCG[k]
				agg.NDCG[k] += v
				ndcgSamples[k] = append(ndcgSamples[k], v)
				agg.NDCGLinear[k] += scores.NDCGLinear[k]
				agg.Precision[k] += scores.Precision[k]
				agg.Recall[k] += scores.Recall[k]
				agg.F1[k] += scores.F1[k]
			}
		}

		if retrievedTopK > 0 {
			coverage := float64(gradedTopK) / float64(retrievedTopK)
			agg.JudgedCoverage = &coverage
		}

		if len(allStats) > 0 {
			agg.Latency = fromRunnerLatencyStats(runner.AggregateLatencyStats(allStats))
		}

		if agg.JudgedCount > 0 {
			n := float64(agg.JudgedCount)
			agg.MAP /= n
			agg.MRR /= n
			agg.MBpref /= n
			for _, k := range kValues {
				agg.NDCG[k] /= n
				agg.NDCGLinear[k] /= n
				agg.Precision[k] /= n
				agg.Recall[k] /= n
				agg.F1[k] /= n
				if samples := ndcgSamples[k]; len(samples) > 1 {
					agg.NDCGStddev[k] = sampleStddev(samples)
				}
			}
		}

		entries = append(entries, agg)
	}

	return entries
}

// computeSignificance compares every engine pair in the job on NDCG@K (all K
// values), MAP, and MRR, over the job's judged queries.
func computeSignificance(scope jobScope) []PairwiseSignificance {
	jr, kValues := scope.job, scope.kValues
	engNames := jr.EngineNames
	var results []PairwiseSignificance

	for i := 0; i < len(engNames); i++ {
		for j := i + 1; j < len(engNames); j++ {
			engA, engB := engNames[i], engNames[j]

			// Collect paired per-query scores for all metrics at once.
			var mapA, mapB, mrrA, mrrB []float64
			ndcgA := make(map[int][]float64, len(kValues))
			ndcgB := make(map[int][]float64, len(kValues))

			for _, qID := range jr.QueryOrder {
				if !scope.judged[qID] {
					continue
				}
				scoresA, scoresB := scope.scoresOf(qID, engA), scope.scoresOf(qID, engB)
				mapA = append(mapA, scoresA.AP)
				mapB = append(mapB, scoresB.AP)
				mrrA = append(mrrA, scoresA.RR)
				mrrB = append(mrrB, scoresB.RR)
				for _, k := range kValues {
					ndcgA[k] = append(ndcgA[k], scoresA.NDCG[k])
					ndcgB[k] = append(ndcgB[k], scoresB.NDCG[k])
				}
			}
			if len(mapA) == 0 {
				continue
			}

			for _, k := range kValues {
				results = append(results, comparePair(pairedScores{
					engineA: engA, engineB: engB, metric: fmt.Sprintf("NDCG@%d", k), a: ndcgA[k], b: ndcgB[k],
				}))
			}
			results = append(results,
				comparePair(pairedScores{engineA: engA, engineB: engB, metric: "MAP", a: mapA, b: mapB}),
				comparePair(pairedScores{engineA: engA, engineB: engB, metric: "MRR", a: mrrA, b: mrrB}),
			)
		}
	}
	adjustAcrossEnginePairs(results)
	return results
}

// pairedScores lines a and b up by query.
type pairedScores struct {
	engineA, engineB, metric string
	a, b                     []float64
}

// comparePair seeds the bootstrap from the pair and metric names, so a report
// regenerated from the same results is identical.
func comparePair(scores pairedScores) PairwiseSignificance {
	differences := make([]float64, len(scores.a))
	for i := range scores.a {
		differences[i] = scores.a[i] - scores.b[i]
	}
	seed := fnv.New64a()
	_, _ = seed.Write([]byte(scores.engineA + "\x00" + scores.engineB + "\x00" + scores.metric))
	meanDifference := metrics.BootstrapMean(differences, seed.Sum64())

	result := PairwiseSignificance{
		EngineA: scores.engineA, EngineB: scores.engineB, Metric: scores.metric,
		MeanDifference: &meanDifference,
	}
	if test := metrics.Wilcoxon(scores.a, scores.b); test != nil {
		result.W, result.P = test.W, test.P
	} else {
		result.Untestable, result.P = true, 1
	}
	return result
}

// adjustAcrossEnginePairs applies the Holm correction to each metric's family
// of engine-pair tests and marks significance from the adjusted p.
func adjustAcrossEnginePairs(results []PairwiseSignificance) {
	byMetric := make(map[string][]int)
	for i, r := range results {
		byMetric[r.Metric] = append(byMetric[r.Metric], i)
	}
	for _, members := range byMetric {
		raw := make([]float64, len(members))
		for i, idx := range members {
			raw[i] = results[idx].P
		}
		for i, adjusted := range metrics.Holm(raw) {
			results[members[i]].AdjustedP = &adjusted
			results[members[i]].Stars = metrics.SignificanceStars(adjusted)
		}
	}
}

func sampleStddev(vals []float64) float64 {
	if len(vals) < 2 {
		return 0
	}
	mean := 0.0
	for _, v := range vals {
		mean += v
	}
	mean /= float64(len(vals))
	variance := 0.0
	for _, v := range vals {
		d := v - mean
		variance += d * d
	}
	return math.Sqrt(variance / float64(len(vals)-1))
}
