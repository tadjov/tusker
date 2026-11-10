package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/judgment"
	"github.com/tadjov/tusker/internal/bench/report"
)

func gradedTrack(t *testing.T) (lineageTrack, uuid.UUID) {
	t.Helper()
	tr := newLineageTrack(t, "http://unused.invalid")
	doc := uuid.New()
	tr.writeJudgments(t, "2026-10-17T10-00-00-pool-aaaaaa", map[string]map[uuid.UUID]int{"q1": {doc: 3}})
	return tr, doc
}

func TestExportQrels_AnnotationsPathAsStrategyWritesQrelsBesideIt(t *testing.T) {
	tr, doc := gradedTrack(t)

	_, err := execute(t, newExportCmd(), tr.dir, "--format", "qrels", "--strategy", tr.judgments)
	require.NoError(t, err)

	_, err = judgment.ReadFile(tr.judgments)
	require.NoError(t, err, "the annotations read must survive the export")
	qrels, err := os.ReadFile(filepath.Join(tr.dir, "trec", "qrels.fixture.tsv"))
	require.NoError(t, err)
	assert.Equal(t, "q1\t0\t"+doc.String()+"\t3\n", string(qrels))
}

func TestExportQrels_RefusesToWriteOverItsInput(t *testing.T) {
	tr, _ := gradedTrack(t)
	before, err := os.ReadFile(tr.judgments)
	require.NoError(t, err)
	sameFileViaSymlinks, err := filepath.EvalSymlinks(tr.judgments)
	require.NoError(t, err)

	_, err = execute(t, newExportCmd(), tr.dir, "--format", "qrels",
		"--judgments", tr.judgments, "--output", sameFileViaSymlinks)

	require.Error(t, err)
	after, err := os.ReadFile(tr.judgments)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// A reader holding the previous qrels keeps a whole file: the export replaces
// it rather than rewriting it in place.
func TestExportQrels_ReplacesTheOldFileWhole(t *testing.T) {
	tr, doc := gradedTrack(t)
	out := filepath.Join(tr.dir, "trec", "qrels.fixture.tsv")
	require.NoError(t, os.WriteFile(out, []byte("q1\t0\told\t1\n"), 0o644))
	held := filepath.Join(t.TempDir(), "held.tsv")
	require.NoError(t, os.Link(out, held))

	_, err := execute(t, newExportCmd(), tr.dir, "--format", "qrels", "--judgments", tr.judgments, "--output", out)
	require.NoError(t, err)

	heldContent, err := os.ReadFile(held)
	require.NoError(t, err)
	assert.Equal(t, "q1\t0\told\t1\n", string(heldContent))
	newContent, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "q1\t0\t"+doc.String()+"\t3\n", string(newContent))
}

func TestExportQrels_JudgmentsPathNamesTheDefaultOutput(t *testing.T) {
	tr, doc := gradedTrack(t)

	_, err := execute(t, newExportCmd(), tr.dir, "--format", "qrels", "--judgments", tr.judgments)
	require.NoError(t, err)

	written, err := filepath.Glob(filepath.Join(tr.dir, "trec", "*.tsv"))
	require.NoError(t, err)
	require.Len(t, written, 1, "one export writes one qrels file")
	assert.Equal(t, "qrels.fixture.tsv", filepath.Base(written[0]))
	qrels, err := os.ReadFile(written[0])
	require.NoError(t, err)
	assert.Equal(t, "q1\t0\t"+doc.String()+"\t3\n", string(qrels))
}

func TestExport_UnknownFormatListsRun(t *testing.T) {
	err := executeExport(newExportCmd(), exportFlags{format: "bogus"}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "run")
}

func TestExport_HelpDescribesRunFormat(t *testing.T) {
	cmd := newExportCmd()

	assert.Contains(t, cmd.Long, "--format run")
	assert.Contains(t, cmd.Flags().Lookup("format").Usage, "run")
}

func TestExport_RunWritesTheLatestReportsRankingsUnderItsRunID(t *testing.T) {
	reports := scaffoldDiffTrack(t)
	first, second := uuid.MustParse("00000000-0000-0000-0000-000000000001"), uuid.MustParse("00000000-0000-0000-0000-000000000002")
	rpt := diffReport(diffRun{runID: "2026-01-01T00-00-01-run-aaaaaa", kValues: []int{10}, ndcg: map[int]float64{10: 0.6}, threshold: 1})
	rpt.Jobs[0].PerQuery[0].Ranking = []report.RankedDoc{{DocID: first, Score: 2}, {DocID: second, Score: 1}}
	reportPath := filepath.Join(reports, rpt.Provenance.RunID+".json")
	require.NoError(t, report.WriteJSON(rpt, reportPath))
	require.NoError(t, updateLatestPointer(filepath.Join(reports, "latest.json"), reportPath))

	cmd := newExportCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"demo", "--format", "run"})
	require.NoError(t, cmd.Execute())

	run, err := os.ReadFile(filepath.Join(reports, "2026-01-01T00-00-01-run-aaaaaa.run", "fts.pg.run"))
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(run)), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, []string{"q1", "Q0", first.String(), "1"}, strings.Fields(lines[0])[:4])
	assert.Equal(t, []string{"q1", "Q0", second.String(), "2"}, strings.Fields(lines[1])[:4])
	assert.Equal(t, "pg", strings.Fields(lines[0])[5])
}
