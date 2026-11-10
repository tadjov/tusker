package report

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/judgment"
	"github.com/tadjov/tusker/internal/bench/metrics"
	"github.com/tadjov/tusker/internal/bench/runner"
)

func fixtureDoc(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n))
}

var trecFixtureGrades = map[string]map[uuid.UUID]int{
	"q1": {fixtureDoc(1): 3, fixtureDoc(2): 0, fixtureDoc(3): 1, fixtureDoc(4): 2, fixtureDoc(5): 0},
	"q2": {fixtureDoc(1): 1, fixtureDoc(6): 2, fixtureDoc(7): 0},
	"q3": {fixtureDoc(2): 2, fixtureDoc(8): 1},
}

// pg fails q3; ranks 4 of pg/q1, 4 of es/q2 and 2 of es/q3 hold ungraded docs.
var trecFixtureRankings = map[string]map[string][]uuid.UUID{
	"q1": {
		"pg": {fixtureDoc(3), fixtureDoc(2), fixtureDoc(1), fixtureDoc(9), fixtureDoc(4)},
		"es": {fixtureDoc(1), fixtureDoc(4), fixtureDoc(3)},
	},
	"q2": {
		"pg": {fixtureDoc(7), fixtureDoc(6)},
		"es": {fixtureDoc(6), fixtureDoc(1), fixtureDoc(7), fixtureDoc(9)},
	},
	"q3": {
		"es": {fixtureDoc(8), fixtureDoc(5), fixtureDoc(2)},
	},
}

func trecFixture() *runner.BenchmarkResult {
	kValues := []int{5, 10}
	cfg := runner.Config{KValues: kValues, RelevanceThreshold: 1, Judgments: map[string]map[string]int{}}
	jr := &runner.JobResult{
		JobName:     "fts",
		EngineNames: []string{"pg", "es"},
		QueryOrder:  []string{"q1", "q2", "q3"},
		Results:     make(map[string]map[string]runner.QueryResult),
	}
	for _, qid := range jr.QueryOrder {
		cfg.Judgments[qid] = make(map[string]int)
		for doc, grade := range trecFixtureGrades[qid] {
			cfg.Judgments[qid][doc.String()] = grade
		}
		jr.Results[qid] = make(map[string]runner.QueryResult)
		for _, engine := range jr.EngineNames {
			ranked, ok := trecFixtureRankings[qid][engine]
			if !ok {
				jr.Results[qid][engine] = runner.QueryResult{QueryID: qid, EngineName: engine, Error: errors.New("timeout")}
				continue
			}
			jr.Results[qid][engine] = runner.QueryResult{
				QueryID:       qid,
				EngineName:    engine,
				Scores:        metrics.ComputeAll(ranked, trecFixtureGrades[qid], kValues, cfg.RelevanceThreshold),
				RankedDocIDs:  ranked,
				ReturnedCount: len(ranked),
			}
		}
	}
	return &runner.BenchmarkResult{Config: cfg, Jobs: []*runner.JobResult{jr}}
}

func trecFixtureQrels(t *testing.T, path string) {
	t.Helper()
	jf := &judgment.File{}
	for _, qid := range []string{"q1", "q2", "q3"} {
		entry := judgment.Entry{QueryID: qid}
		for n := 1; n <= 9; n++ {
			if grade, ok := trecFixtureGrades[qid][fixtureDoc(n)]; ok {
				entry.Docs = append(entry.Docs, judgment.GradedDoc{DocID: fixtureDoc(n), Grade: grade})
			}
		}
		jf.Queries = append(jf.Queries, entry)
	}
	require.NoError(t, judgment.WriteQrels(jf, path))
}

// assertRunMatchesRankings checks the run lists each answered query's docs in
// the fixture's order with ranks 1..n and strictly decreasing scores, the only
// things trec_eval reads. A failed query has no lines.
func assertRunMatchesRankings(t *testing.T, run, engine string) {
	t.Helper()
	type line struct {
		docID string
		rank  string
		score float64
	}
	byQuery := make(map[string][]line)
	for _, text := range strings.Split(strings.TrimSpace(run), "\n") {
		fields := strings.Fields(text)
		require.Len(t, fields, 6, text)
		assert.Equal(t, "Q0", fields[1])
		assert.Equal(t, engine, fields[5])
		score, err := strconv.ParseFloat(fields[4], 64)
		require.NoError(t, err, text)
		byQuery[fields[0]] = append(byQuery[fields[0]], line{docID: fields[2], rank: fields[3], score: score})
	}

	for qid, rankings := range trecFixtureRankings {
		ranked, answered := rankings[engine]
		if !answered {
			assert.NotContains(t, byQuery, qid, "%s failed %s, so the run has no lines for it", engine, qid)
			continue
		}
		got := byQuery[qid]
		require.Len(t, got, len(ranked), "%s %s", engine, qid)
		for i, docID := range ranked {
			assert.Equal(t, docID.String(), got[i].docID, "%s %s rank %d", engine, qid, i+1)
			assert.Equal(t, strconv.Itoa(i+1), got[i].rank)
			if i > 0 {
				assert.Less(t, got[i].score, got[i-1].score, "%s %s scores must strictly decrease", engine, qid)
			}
		}
	}
}

// The expected values are trec_eval 10.0 output for
// `trec_eval -c -l 1 -m map -m P.5,10 -m ndcg_cut.5,10 qrels <engine>.run`, on the qrels
// trecFixtureQrels writes and the run files below.
func TestWriteTRECRun_ReproducesReportMetricsUnderTrecEval(t *testing.T) {
	r := Generate(trecFixture(), nil)
	job := &r.Jobs[0]

	trecEval := map[string]struct{ mapScore, p5, p10, ndcgCut5, ndcgCut10 float64 }{
		"pg": {mapScore: 0.3352, p5: 0.2667, p10: 0.1333, ndcgCut5: 0.3890, ndcgCut10: 0.3890},
		"es": {mapScore: 0.9444, p5: 0.4667, p10: 0.2333, ndcgCut5: 0.9201, ndcgCut10: 0.9201},
	}
	for _, agg := range job.Aggregated {
		want := trecEval[agg.EngineName]
		assert.InDelta(t, want.mapScore, agg.MAP, 5e-5, agg.EngineName)
		assert.InDelta(t, want.p5, agg.Precision[5], 5e-5, agg.EngineName)
		assert.InDelta(t, want.p10, agg.Precision[10], 5e-5, agg.EngineName)
		assert.InDelta(t, want.ndcgCut5, agg.NDCGLinear[5], 5e-5, agg.EngineName)
		assert.InDelta(t, want.ndcgCut10, agg.NDCGLinear[10], 5e-5, agg.EngineName)
	}

	for _, engine := range []string{"pg", "es"} {
		var run bytes.Buffer
		require.NoError(t, WriteTRECRun(&run, job, engine))
		assertRunMatchesRankings(t, run.String(), engine)
	}

	qrelsPath := filepath.Join(t.TempDir(), "qrels.tsv")
	trecFixtureQrels(t, qrelsPath)
	qrels, err := os.ReadFile(qrelsPath)
	require.NoError(t, err)
	assert.Contains(t, string(qrels), "q3\t0\t00000000-0000-0000-0000-000000000008\t1\n",
		"run and qrels name queries and docs the same way")
}

func TestReadJSON_KeepsRankings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	require.NoError(t, WriteJSON(Generate(trecFixture(), nil), path))

	r, err := ReadJSON(path)
	require.NoError(t, err)

	var before, after bytes.Buffer
	require.NoError(t, WriteTRECRun(&before, &Generate(trecFixture(), nil).Jobs[0], "es"))
	require.NoError(t, WriteTRECRun(&after, &r.Jobs[0], "es"))
	require.NotEmpty(t, after.String())
	assert.Equal(t, before.String(), after.String())
}

func TestWriteTRECRuns_WritesOneFilePerJobAndEngine(t *testing.T) {
	r := Generate(trecFixture(), nil)
	dir := filepath.Join(t.TempDir(), "runs")

	paths, err := WriteTRECRuns(r, dir)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{filepath.Join(dir, "fts.pg.run"), filepath.Join(dir, "fts.es.run")}, paths)
	var want bytes.Buffer
	require.NoError(t, WriteTRECRun(&want, &r.Jobs[0], "es"))
	got, err := os.ReadFile(filepath.Join(dir, "fts.es.run"))
	require.NoError(t, err)
	assert.Equal(t, want.String(), string(got))
}

func TestWriteTRECRuns_RefusesAReportWithoutRankings(t *testing.T) {
	r := Generate(trecFixture(), nil)
	for i := range r.Jobs[0].PerQuery {
		r.Jobs[0].PerQuery[i].Ranking = nil
	}

	_, err := WriteTRECRuns(r, t.TempDir())

	assert.Error(t, err)
}
