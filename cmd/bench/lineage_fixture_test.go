package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DjordjeVuckovic/tusker/internal/bench/judgment"
	"github.com/DjordjeVuckovic/tusker/internal/bench/meta"
	"github.com/DjordjeVuckovic/tusker/internal/bench/pool"
	"github.com/DjordjeVuckovic/tusker/internal/bench/report"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// fakeIndex is one engine's index in fakeCluster: its document count and the
// ranked ids each query returns. A query listed in failing answers 500.
type fakeIndex struct {
	count   int64
	ranked  map[string][]uuid.UUID
	failing map[string]bool
}

// fakeCluster serves the part of the Elasticsearch API the bench executors
// call, so pool and run go end to end against engines whose answers the test
// fixes. Each suite query carries its own id as the match text.
func fakeCluster(t *testing.T, indexes map[string]*fakeIndex) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		if parts[0] == "_scripts" && r.Method == http.MethodPut {
			_, _ = io.WriteString(w, `{"acknowledged": true}`)
			return
		}
		index, ok := indexes[parts[0]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch strings.Join(parts[1:], "/") {
		case "_count":
			_, _ = fmt.Fprintf(w, `{"count": %d}`, index.count)
		case "_search", "_search/template":
			body, _ := io.ReadAll(r.Body)
			for queryID, ids := range index.ranked {
				if !bytes.Contains(body, []byte(`"`+queryID+`"`)) {
					continue
				}
				if index.failing[queryID] {
					http.Error(w, "shard failure", http.StatusInternalServerError)
					return
				}
				writeHits(w, ids)
				return
			}
			writeHits(w, nil)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeHits(w io.Writer, ids []uuid.UUID) {
	hits := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		hits = append(hits, map[string]any{"_source": map[string]string{"id": id.String()}})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"hits": map[string]any{"total": map[string]any{"value": len(ids), "relation": "eq"}, "hits": hits},
	})
}

// lineageTrack is a track with engines a and b over queries q1 and q2, in two
// jobs: "both" and "a-only".
type lineageTrack struct {
	dir       string
	judgments string
}

func newLineageTrack(t *testing.T, clusterURL string) lineageTrack {
	t.Helper()
	t.Setenv("EMBEDDING_BASE_URL", "")
	dir := t.TempDir()
	tr := lineageTrack{dir: dir, judgments: filepath.Join(dir, "trec", "annotations.fixture.yaml")}
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "trec"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "spec.yaml"), fmt.Appendf(nil, `schema_version: 1
id: lineage
kind: fts
defaults:
  judgments: %q
engines:
  a:
    type: elasticsearch
    connection: %q
    index: a
    params:
      boost: 2
  b:
    type: elasticsearch
    connection: %q
    index: b
metrics:
  k_values: [1, 2]
  relevance_threshold: 1
runs:
  warmup: 1
  iterations: 2
jobs:
  - name: both
    suite: suite.yaml
    engines: [a, b]
  - name: a-only
    suite: suite.yaml
    engines: [a]
`, tr.judgments, clusterURL, clusterURL), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "suite.yaml"), []byte(`schema_version: 1
id: lineage
queries:
  - id: q1
    engines:
      a: { query: '{"query": {"match": {"title": "q1"}}}' }
      b: { query: '{"query": {"match": {"title": "q1"}}}' }
  - id: q2
    engines:
      a: { query: '{"query": {"match": {"title": "q2"}}}' }
      b: { query: '{"query": {"match": {"title": "q2"}}}' }
`), 0o644))
	return tr
}

func (tr lineageTrack) poolPath() string { return filepath.Join(tr.dir, "trec", "pool.yaml") }

// writePool stands in for a bench pool run over engines a and b that produced
// runID and pooled docs for q1 and q2.
func (tr lineageTrack) writePool(t *testing.T, runID string, docs map[string][]uuid.UUID) {
	t.Helper()
	pf := &pool.PoolFile{Meta: meta.Meta{RunID: runID, Engines: []string{"a", "b"}}}
	for _, queryID := range []string{"q1", "q2"} {
		entry := pool.PoolEntry{QueryID: queryID}
		for _, id := range docs[queryID] {
			entry.Docs = append(entry.Docs, pool.PooledDoc{DocID: id, Sources: []string{"a"}})
		}
		pf.Queries = append(pf.Queries, entry)
	}
	require.NoError(t, pool.WritePoolFile(pf, tr.poolPath()))
}

// pool runs bench pool over the track and returns the pool it wrote.
func (tr lineageTrack) pool(t *testing.T, args ...string) *pool.PoolFile {
	t.Helper()
	_, err := execute(t, newPoolCmd(), append([]string{tr.dir}, args...)...)
	require.NoError(t, err)
	pf, err := pool.ReadPoolFile(tr.poolPath())
	require.NoError(t, err)
	return pf
}

func (tr lineageTrack) poolRunID(t *testing.T) string {
	t.Helper()
	pf, err := pool.ReadPoolFile(tr.poolPath())
	require.NoError(t, err)
	return pf.Meta.RunID
}

// judgeEveryPooledDoc grades every doc of pf with grade.
func (tr lineageTrack) judgeEveryPooledDoc(t *testing.T, pf *pool.PoolFile, grade int) {
	t.Helper()
	grades := make(map[string]map[uuid.UUID]int)
	for _, entry := range pf.Queries {
		grades[entry.QueryID] = make(map[uuid.UUID]int)
		for _, doc := range entry.Docs {
			grades[entry.QueryID][doc.DocID] = grade
		}
	}
	tr.writeJudgments(t, pf.Meta.RunID, grades)
}

// writeJudgments grades every listed doc against the pool with poolRunID. A
// query absent from grades has no entry, as when the judge never reached it.
func (tr lineageTrack) writeJudgments(t *testing.T, poolRunID string, grades map[string]map[uuid.UUID]int) {
	t.Helper()
	jf := &judgment.File{Meta: meta.New("judge")}
	jf.Meta.Judge = &meta.Judge{Strategy: "fixture"}
	jf.Meta.PoolRunID = poolRunID
	for _, queryID := range []string{"q1", "q2"} {
		if _, ok := grades[queryID]; !ok {
			continue
		}
		entry := judgment.Entry{QueryID: queryID}
		for id, grade := range grades[queryID] {
			entry.Docs = append(entry.Docs, judgment.GradedDoc{DocID: id, Grade: grade})
		}
		jf.Queries = append(jf.Queries, entry)
	}
	require.NoError(t, judgment.WriteFile(jf, tr.judgments))
}

func (tr lineageTrack) reports(t *testing.T) []*report.Report {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(tr.dir, "reports", "*-run-*.json"))
	require.NoError(t, err)
	out := make([]*report.Report, 0, len(paths))
	for _, p := range paths {
		rpt, err := report.ReadJSON(p)
		require.NoError(t, err)
		out = append(out, rpt)
	}
	return out
}

func execute(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func (tr lineageTrack) latestPath() string { return filepath.Join(tr.dir, "reports", "latest.json") }

// latestReport returns the report latest.json points at, or nil when there is
// no pointer.
func (tr lineageTrack) latestReport(t *testing.T) *report.Report {
	t.Helper()
	if _, err := os.Stat(tr.latestPath()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	rpt, err := report.ReadLatestReport(tr.latestPath())
	require.NoError(t, err)
	return rpt
}
