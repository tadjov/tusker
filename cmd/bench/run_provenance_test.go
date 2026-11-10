package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/DjordjeVuckovic/tusker/internal/bench/judgment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_ReportNamesThePoolAndJudgmentsItScoredAgainst(t *testing.T) {
	tr := judgedTrack(t, nil)
	jf, err := judgment.ReadFile(tr.judgments)
	require.NoError(t, err)

	_, err = execute(t, newRunCmd(), tr.dir)
	require.NoError(t, err)

	provenance := tr.latestReport(t).Provenance
	assert.Equal(t, tr.poolRunID(t), provenance.PoolRunID)
	assert.Equal(t, jf.Meta.RunID, provenance.JudgmentsRunID)
}

// twoSuiteTrack is the judged track with job a-only reading its own copy of
// the suite, suite-a.yaml, whose path it returns.
func twoSuiteTrack(t *testing.T) (lineageTrack, string) {
	t.Helper()
	tr := judgedTrack(t, nil)
	aOnlySuite := copyFile(t, filepath.Join(tr.dir, "suite.yaml"), filepath.Join(tr.dir, "suite-a.yaml"))
	editFile(t, filepath.Join(tr.dir, "spec.yaml"), "  - name: a-only\n    suite: suite.yaml", "  - name: a-only\n    suite: suite-a.yaml")
	return tr, aOnlySuite
}

func TestRun_ReportListsTheSuitesItsJobsLoaded(t *testing.T) {
	tr, aOnlySuite := twoSuiteTrack(t)

	_, err := execute(t, newRunCmd(), tr.dir)
	require.NoError(t, err)
	_, err = execute(t, newRunCmd(), tr.dir, "--jobs", "a-only")
	require.NoError(t, err)

	suitesByJobs := map[int][]string{}
	for _, rpt := range tr.reports(t) {
		suitesByJobs[len(rpt.Jobs)] = rpt.Provenance.Sources.Suites
	}
	resolved, err := filepath.EvalSymlinks(aOnlySuite)
	require.NoError(t, err)
	both := filepath.Join(filepath.Dir(resolved), "suite.yaml")
	assert.ElementsMatch(t, []string{both, resolved}, canonicalPaths(t, suitesByJobs[2]), "every job's suite")
	assert.Equal(t, []string{resolved}, canonicalPaths(t, suitesByJobs[1]), "only the suite of the job that ran")
}

func canonicalPaths(t *testing.T, paths []string) []string {
	t.Helper()
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		resolved, err := filepath.EvalSymlinks(p)
		require.NoError(t, err)
		out = append(out, resolved)
	}
	return out
}

func TestShowReport_NamesTheSuitesTheRunLoaded(t *testing.T) {
	tr, _ := twoSuiteTrack(t)
	_, err := execute(t, newRunCmd(), tr.dir)
	require.NoError(t, err)

	var out bytes.Buffer
	showReport(&out, tr.latestReport(t))

	assert.Contains(t, out.String(), "suite.yaml")
	assert.Contains(t, out.String(), "suite-a.yaml")
}

func TestExportHTML_NamesTheSuitesTheRunLoaded(t *testing.T) {
	tr, _ := twoSuiteTrack(t)
	_, err := execute(t, newRunCmd(), tr.dir)
	require.NoError(t, err)

	_, err = execute(t, newExportCmd(), tr.dir, "--format", "html")
	require.NoError(t, err)

	pages, err := filepath.Glob(filepath.Join(tr.dir, "reports", "*.html"))
	require.NoError(t, err)
	require.Len(t, pages, 1)
	page, err := os.ReadFile(pages[0])
	require.NoError(t, err)
	assert.Contains(t, string(page), "suite.yaml")
	assert.Contains(t, string(page), "suite-a.yaml")
}
