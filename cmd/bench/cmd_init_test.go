package main

import (
	"bytes"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/DjordjeVuckovic/tusker/internal/bench/runner"
	"github.com/DjordjeVuckovic/tusker/internal/bench/spec"
	"github.com/DjordjeVuckovic/tusker/internal/bench/suite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInit_ScaffoldedSpecResolvesItsSuite(t *testing.T) {
	tests := []struct {
		name      string
		trackRoot func(cwd string) string
		trackDir  string
	}{
		{name: "track root defaults to cwd", trackRoot: func(string) string { return "" }, trackDir: "cc-news/fts"},
		{name: "relative track root", trackRoot: func(string) string { return "tracks" }, trackDir: "tracks/cc-news/fts"},
		{name: "absolute track root", trackRoot: func(cwd string) string { return cwd }, trackDir: "cc-news/fts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir()
			t.Chdir(cwd)
			prevRoot := trackRoot
			trackRoot = tt.trackRoot(cwd)
			t.Cleanup(func() { trackRoot = prevRoot })

			var out bytes.Buffer
			cmd := newInitCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"cc-news/fts"})
			require.NoError(t, cmd.Execute())

			bs, err := spec.LoadFromFile(filepath.Join(cwd, tt.trackDir, "spec.yaml"))
			require.NoError(t, err)
			require.NotEmpty(t, bs.Jobs)

			_, err = runner.LoadSuites(bs)
			require.NoError(t, err)
			for name, eng := range bs.Engines {
				if eng.Type == spec.EnginePostgres {
					assert.Equal(t, "force_custom_plan", eng.ConnectionSettings["plan_cache_mode"], name)
				}
			}
		})
	}
}

// Equal scores tie in plan order unless id breaks them, so a run could rank
// docs the pool never saw.
func TestInit_ScaffoldedPostgresQueriesBreakScoreTiesByID(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	prevRoot := trackRoot
	trackRoot = ""
	t.Cleanup(func() { trackRoot = prevRoot })
	cmd := newInitCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"cc-news/fts"})
	require.NoError(t, cmd.Execute())
	bs, err := spec.LoadFromFile(filepath.Join(cwd, "cc-news/fts", "spec.yaml"))
	require.NoError(t, err)
	suites, err := runner.LoadSuites(bs)
	require.NoError(t, err)

	tieBreak := regexp.MustCompile(`(?is)ORDER BY.*\bDESC\s*,\s*id\b`)
	checked := 0
	for _, job := range bs.Jobs {
		ls := suites[job.Suite]
		for _, engineName := range job.Engines {
			if bs.Engines[engineName].Type != spec.EnginePostgres {
				continue
			}
			for _, q := range ls.Suite.Queries {
				resolved, err := q.ResolveEngineQuery(suite.ResolveOptions{Engine: engineName, Registry: ls.Registry, SuiteDir: ls.Dir})
				require.NoError(t, err)
				assert.Regexp(t, tieBreak, resolved.Query, "query %s engine %s", q.ID, engineName)
				checked++
			}
		}
	}
	require.NotZero(t, checked, "the scaffold has a postgres query")
}
