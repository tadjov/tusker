package report

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/metrics"
	"github.com/tadjov/tusker/internal/bench/spec"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/es"
	"github.com/tadjov/tusker/internal/storage/pg"
)

func TestGenerate_RecordsIndexProvenanceThroughTheWrittenReport(t *testing.T) {
	br := makeBenchmarkResult([]string{"pg", "es"}, []string{"q1"}, map[string]map[string]metrics.ScoreSet{
		"q1": {"pg": makeJudgedScores(0.7), "es": makeJudgedScores(0.5)},
	})
	rpt := Generate(br, &GenerateOptions{
		Spec: &spec.BenchSpec{Engines: map[string]spec.Engine{
			"pg":  {Type: "postgres"},
			"es":  {Type: "elasticsearch"},
			"api": {Type: "api"},
		}},
		Indexes: map[string]IndexProvenance{
			"pg": {IndexDescription: engine.IndexDescription{Postgres: &pg.IndexDescription{
				ServerVersion: "18.0",
				Settings:      map[string]string{"hnsw.ef_search": "200"},
			}}},
			"es": {IndexDescription: engine.IndexDescription{Elasticsearch: &es.IndexDescription{
				Version: "8.15.0",
				Index:   "news-cc-000001",
			}}},
			"api": {Error: "engine cannot describe its index"},
		},
	})

	path := filepath.Join(t.TempDir(), "report.json")
	if err := WriteJSON(rpt, path); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	got, err := ReadJSON(path)
	if err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}

	engines := got.Environment.Engines
	if v := engines["pg"].Version; v != "18.0" {
		t.Errorf("pg version = %q, want 18.0", v)
	}
	if v := engines["es"].Version; v != "8.15.0" {
		t.Errorf("es version = %q, want 8.15.0", v)
	}
	if idx := engines["pg"].Index; idx == nil || idx.Postgres == nil || idx.Postgres.Settings["hnsw.ef_search"] != "200" {
		t.Errorf("pg index provenance = %+v, want the ef_search it was measured at", idx)
	}
	if idx := engines["es"].Index; idx == nil || idx.Elasticsearch == nil || idx.Elasticsearch.Index != "news-cc-000001" {
		t.Errorf("es index provenance = %+v, want the concrete index", idx)
	}
	if idx := engines["api"].Index; idx == nil || idx.Error == "" {
		t.Errorf("api index provenance = %+v, want a block carrying the error", idx)
	}
}

func reportWithLoads(pgEmbeddings, esEmbeddings string) *Report {
	load := func(engineName, embeddingsVersion string) *storage.LoadRecord {
		return &storage.LoadRecord{
			Engine:     engineName,
			Canonical:  storage.PackageRef{Name: "cc-news", Version: "article-v2-601d5476"},
			Embeddings: &storage.PackageRef{Name: "cc-news-embeddings", Version: embeddingsVersion},
			ModelTag:   "qwen3-embedding:0.6b@1024",
		}
	}
	return &Report{Environment: RunEnvironment{Engines: map[string]EngineInfo{
		"pg": {Index: &IndexProvenance{IndexDescription: engine.IndexDescription{Postgres: &pg.IndexDescription{
			ServerVersion: "18.0", Load: load("pg-native", pgEmbeddings),
		}}}},
		"es": {Index: &IndexProvenance{IndexDescription: engine.IndexDescription{Elasticsearch: &es.IndexDescription{
			Version: "8.12.0", Index: "articles", Load: load("elasticsearch", esEmbeddings),
		}}}},
	}}}
}

func TestDiffProvenance_NamesALoadOfOtherEmbeddings(t *testing.T) {
	before := reportWithLoads("lead-v1-1024-938b2254", "lead-v1-1024-938b2254")
	after := reportWithLoads("lead-v1-1024-0badc0de", "lead-v1-1024-0badc0de")

	changes := DiffProvenance(before, after)

	for _, engineName := range []string{"es", "pg"} {
		found := false
		for _, c := range changes {
			if c.Engine == engineName && c.Before == "lead-v1-1024-938b2254" && c.After == "lead-v1-1024-0badc0de" {
				found = true
			}
		}
		if !found {
			t.Errorf("changes = %+v, want %s's embeddings version to differ", changes, engineName)
		}
	}
	if same := DiffProvenance(before, before); len(same) != 0 {
		t.Errorf("identical loads differ: %+v", same)
	}
}

func TestSummary_NamesTheLoadedPackagesAndTag(t *testing.T) {
	rpt := reportWithLoads("lead-v1-1024-938b2254", "lead-v1-1024-938b2254")

	for name, info := range rpt.Environment.Engines {
		summary := info.Index.Summary()
		for _, want := range []string{"article-v2-601d5476", "lead-v1-1024-938b2254", "qwen3-embedding:0.6b@1024"} {
			if !strings.Contains(summary, want) {
				t.Errorf("%s summary = %q, want it to name %s", name, summary, want)
			}
		}
	}
}
