package spec

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validSpecHeader = `schema_version: 1
id: test_spec
`

func TestParse(t *testing.T) {
	t.Run("valid spec", func(t *testing.T) {
		yaml := validSpecHeader + `engines:
  pg-native:
    type: postgres
    connection: "postgresql://localhost/test"
  elasticsearch:
    type: elasticsearch
    connection: "http://localhost:9200"
    index: news

metrics:
  k_values: [3, 5, 10]
  relevance_threshold: 1

runs:
  warmup: 1
  iterations: 3

jobs:
  - name: "raw-comparison"
    suite: tracks/test/suite.yaml
    engines: [pg-native, elasticsearch]
`
		s, err := Parse([]byte(yaml))
		require.NoError(t, err)
		assert.Equal(t, "test_spec", s.ID)
		require.NotEmpty(t, s.Jobs)
		assert.Equal(t, "raw-comparison", s.Jobs[0].Name)
		assert.Equal(t, EnginePostgres, s.Engines["pg-native"].Type)
		assert.Equal(t, "news", s.Engines["elasticsearch"].Index)
		assert.Equal(t, 3, s.Runs.Iterations)
	})

	t.Run("no jobs", func(t *testing.T) {
		yaml := validSpecHeader + `engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
jobs: []
`
		_, err := Parse([]byte(yaml))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no jobs")
	})

	t.Run("no engines", func(t *testing.T) {
		yaml := validSpecHeader + `engines: {}
jobs:
  - name: test
    suite: suite.yaml
    engines: [pg]
`
		_, err := Parse([]byte(yaml))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no engines")
	})

	t.Run("job references unknown engine", func(t *testing.T) {
		yaml := validSpecHeader + `engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
jobs:
  - name: test
    suite: suite.yaml
    engines: [pg, unknown]
`
		_, err := Parse([]byte(yaml))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown engine")
	})

	t.Run("defaults applied", func(t *testing.T) {
		yaml := validSpecHeader + `engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
metrics: {}
runs: {}
jobs:
  - name: test
    suite: suite.yaml
    engines: [pg]
`
		s, err := Parse([]byte(yaml))
		require.NoError(t, err)
		assert.Equal(t, []int{3, 5, 10}, s.Metrics.KValues)
		assert.Equal(t, 1, s.Metrics.RelevanceThreshold)
		assert.Equal(t, 1, s.Runs.Warmup)
		assert.Equal(t, 3, s.Runs.Iterations)
		assert.Equal(t, 30*time.Second, s.Runs.QueryTimeout)
	})

	t.Run("query timeout", func(t *testing.T) {
		yaml := validSpecHeader + `engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
runs:
  query_timeout: 2m30s
jobs:
  - name: test
    suite: suite.yaml
    engines: [pg]
`
		s, err := Parse([]byte(yaml))
		require.NoError(t, err)
		assert.Equal(t, 150*time.Second, s.Runs.QueryTimeout)
	})

	t.Run("negative query timeout", func(t *testing.T) {
		yaml := validSpecHeader + `engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
runs:
  query_timeout: -5s
jobs:
  - name: test
    suite: suite.yaml
    engines: [pg]
`
		_, err := Parse([]byte(yaml))
		assert.ErrorContains(t, err, "runs.query_timeout")
	})

	t.Run("invalid engine type", func(t *testing.T) {
		yaml := validSpecHeader + `engines:
  bad:
    type: mysql
    connection: "mysql://localhost"
jobs:
  - name: test
    suite: suite.yaml
    engines: [bad]
`
		_, err := Parse([]byte(yaml))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid type")
	})

	t.Run("api engine type is valid", func(t *testing.T) {
		yaml := validSpecHeader + `engines:
  api:
    type: api
    connection: "http://localhost:8080"
jobs:
  - name: api-test
    suite: suite.yaml
    engines: [api]
`
		s, err := Parse([]byte(yaml))
		require.NoError(t, err)
		assert.Equal(t, EngineAPI, s.Engines["api"].Type)
		assert.Equal(t, "http://localhost:8080", s.Engines["api"].Connection)
	})
}

func TestParse_RejectsMissingSchemaVersion(t *testing.T) {
	yaml := `id: test
engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
jobs:
  - name: test
    suite: suite.yaml
    engines: [pg]
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing schema_version")
}

func TestParse_RejectsMissingID(t *testing.T) {
	yaml := `schema_version: 1
engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
jobs:
  - name: test
    suite: suite.yaml
    engines: [pg]
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required field: id")
}

func TestParse_DefaultsBlock(t *testing.T) {
	prev := KnownStrategies
	KnownStrategies = func() []string { return []string{"lexical", "claude-api"} }
	defer func() { KnownStrategies = prev }()

	yaml := validSpecHeader + `defaults:
  pool_depth: 50
  judgments: lexical
engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
jobs:
  - name: test
    suite: suite.yaml
    engines: [pg]
`
	s, err := Parse([]byte(yaml))
	require.NoError(t, err)
	assert.Equal(t, 50, s.Defaults.PoolDepth)
	assert.Equal(t, "lexical", s.Defaults.Judgments)
}

func TestParse_RejectsUnknownDefaultsJudgments(t *testing.T) {
	prev := KnownStrategies
	KnownStrategies = func() []string { return []string{"lexical", "claude-api"} }
	defer func() { KnownStrategies = prev }()

	yaml := validSpecHeader + `defaults:
  judgments: calude-api
engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
jobs:
  - name: test
    suite: suite.yaml
    engines: [pg]
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a known strategy")
}

func TestParse_PathLikeDefaultsJudgmentsAllowed(t *testing.T) {
	prev := KnownStrategies
	KnownStrategies = func() []string { return []string{"lexical"} }
	defer func() { KnownStrategies = prev }()

	yaml := validSpecHeader + `defaults:
  judgments: /tmp/some/ad-hoc.yaml
engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
jobs:
  - name: test
    suite: suite.yaml
    engines: [pg]
`
	s, err := Parse([]byte(yaml))
	require.NoError(t, err)
	assert.Equal(t, "/tmp/some/ad-hoc.yaml", s.Defaults.Judgments)
}

func TestLoadFromFile_ResolvesJobSuitePathsAgainstSpecDir(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.yaml")
	yaml := `schema_version: 1
id: t
engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
jobs:
  - name: rel
    suite: suite.yaml
    engines: [pg]
  - name: abs
    suite: /abs/path/suite.yaml
    engines: [pg]
`
	require.NoError(t, os.WriteFile(specPath, []byte(yaml), 0644))

	cwd, _ := os.Getwd()
	defer func() { _ = os.Chdir(cwd) }()
	// Move CWD elsewhere — proves resolution is anchored to spec dir, not CWD.
	require.NoError(t, os.Chdir(os.TempDir()))

	s, err := LoadFromFile(specPath)
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(dir, "suite.yaml"), s.Jobs[0].Suite,
		"relative suite path must be anchored at the spec file's dir")
	assert.Equal(t, "/abs/path/suite.yaml", s.Jobs[1].Suite,
		"absolute suite paths must be left alone")
}

func TestParse_RejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name, body, key string
	}{
		{
			name: "misspelt metrics key",
			body: `engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
metrics:
  relevance_treshold: 2
jobs:
  - name: test
    suite: suite.yaml
    engines: [pg]
`,
			key: "relevance_treshold",
		},
		{
			name: "misspelt engine key",
			body: `engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
    connection_setings:
      hnsw.ef_search: "100"
jobs:
  - name: test
    suite: suite.yaml
    engines: [pg]
`,
			key: "connection_setings",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(validSpecHeader + tc.body))
			assert.ErrorContains(t, err, tc.key)
		})
	}
}
