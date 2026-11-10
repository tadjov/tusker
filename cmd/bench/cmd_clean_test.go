package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/meta"
	"github.com/tadjov/tusker/internal/bench/report"
	"github.com/tadjov/tusker/internal/bench/version"
)

func writeCleanReport(t *testing.T, tr lineageTrack, runID string, incomplete ...string) string {
	t.Helper()
	path := filepath.Join(tr.dir, "reports", runID+".json")
	rpt := &report.Report{
		SchemaVersion: version.SchemaVersion,
		Provenance:    meta.Meta{RunID: runID},
		Settings:      report.RunSettings{Incomplete: incomplete},
	}
	require.NoError(t, report.WriteJSON(rpt, path))
	return path
}

func TestClean_KeepsLatestAndDropsIncompleteRunsFirst(t *testing.T) {
	tests := []struct {
		name     string
		keep     int
		wantKept []string
	}{
		{name: "keep two", keep: 2, wantKept: []string{"r2-latest", "r4-complete"}},
		{name: "keep three", keep: 3, wantKept: []string{"r1-complete", "r2-latest", "r4-complete"}},
		{name: "keep four", keep: 4, wantKept: []string{"r1-complete", "r2-latest", "r4-complete", "r5-latency-only"}},
		{name: "keep none", keep: 0, wantKept: []string{"r2-latest"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newLineageTrack(t, "http://unused.invalid")
			paths := map[string]string{
				"r1-complete":     writeCleanReport(t, tr, "2026-10-01T00-00-00-run-r1-complete"),
				"r2-latest":       writeCleanReport(t, tr, "2026-10-02T00-00-00-run-r2-latest"),
				"r3-errored":      writeCleanReport(t, tr, "2026-10-03T00-00-00-run-r3-errored", "1 (query, engine) executions errored"),
				"r4-complete":     writeCleanReport(t, tr, "2026-10-04T00-00-00-run-r4-complete"),
				"r5-latency-only": writeCleanReport(t, tr, "2026-10-05T00-00-00-run-r5-latency-only", "latency-only run"),
			}
			require.NoError(t, updateLatestPointer(tr.latestPath(), paths["r2-latest"]))

			_, err := execute(t, newCleanCmd(), tr.dir, "--keep", strconv.Itoa(tt.keep))
			require.NoError(t, err)

			var kept []string
			for name, path := range paths {
				if _, err := os.Stat(path); err == nil {
					kept = append(kept, name)
				}
			}
			assert.ElementsMatch(t, tt.wantKept, kept)
			assert.FileExists(t, tr.latestPath())
		})
	}
}

func TestClean_RejectsNegativeKeep(t *testing.T) {
	tr := newLineageTrack(t, "http://unused.invalid")
	kept := writeCleanReport(t, tr, "2026-10-01T00-00-00-run-r1-complete")
	newer := writeCleanReport(t, tr, "2026-10-02T00-00-00-run-r2-complete")
	require.NoError(t, updateLatestPointer(tr.latestPath(), newer))

	_, err := execute(t, newCleanCmd(), tr.dir, "--keep=-1")

	require.Error(t, err)
	assert.FileExists(t, kept)
	assert.FileExists(t, newer)
}

// Reports written before runs recorded settings.incomplete still show their
// errored executions in the per-engine error counts.
func TestClean_ReportWithoutSettingsThatErroredIsIncomplete(t *testing.T) {
	tr := newLineageTrack(t, "http://unused.invalid")
	complete := writeCleanReport(t, tr, "2026-10-01T00-00-00-run-r1-complete")
	errored := filepath.Join(tr.dir, "reports", "2026-10-02T00-00-00-run-r2-errored.json")
	require.NoError(t, os.WriteFile(errored, []byte(`{"schema_version": 1, "provenance": {"run_id": "r2"},
  "jobs": [{"JobName": "both", "Aggregated": [{"EngineName": "b", "QueryCount": 2, "ErrorCount": 1}]}]}`), 0o644))

	_, err := execute(t, newCleanCmd(), tr.dir, "--keep=1")
	require.NoError(t, err)

	assert.FileExists(t, complete)
	assert.NoFileExists(t, errored)
}

func TestClean_RemovesARunsExportedRunFilesWithItsReport(t *testing.T) {
	tr := newLineageTrack(t, "http://unused.invalid")
	stale := writeCleanReport(t, tr, "2026-10-01T00-00-00-run-r1-complete")
	latest := writeCleanReport(t, tr, "2026-10-02T00-00-00-run-r2-complete")
	require.NoError(t, updateLatestPointer(tr.latestPath(), latest))
	runDirOf := func(reportPath string) string { return strings.TrimSuffix(reportPath, ".json") + ".run" }
	for _, path := range []string{stale, latest} {
		require.NoError(t, os.MkdirAll(runDirOf(path), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(runDirOf(path), "fts.pg.run"), []byte("q1 Q0 d 1 1 pg\n"), 0o644))
	}

	_, err := execute(t, newCleanCmd(), tr.dir, "--keep", "1")
	require.NoError(t, err)

	assert.NoFileExists(t, stale)
	assert.NoDirExists(t, runDirOf(stale))
	assert.FileExists(t, filepath.Join(runDirOf(latest), "fts.pg.run"))
}
