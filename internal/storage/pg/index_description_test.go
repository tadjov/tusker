package pg

import (
	"strings"
	"testing"

	"github.com/tadjov/tusker/internal/storage"
)

func TestDescribeIndexes_ReadsTheMigratedServer(t *testing.T) {
	specSettings := map[string]string{
		"hnsw.ef_search":  "123",
		"plan_cache_mode": "force_custom_plan",
	}
	pool, err := NewConnectionPool(testCtx, PoolConfig{
		ConnStr:            testPool.GetConn().Config().ConnString(),
		ConnectionSettings: specSettings,
	})
	if err != nil {
		t.Fatalf("open pool with connection settings: %v", err)
	}
	t.Cleanup(pool.Close)

	description, err := DescribeIndexes(testCtx, pool)
	if err != nil {
		t.Fatalf("DescribeIndexes: %v", err)
	}

	t.Run("server version", func(t *testing.T) {
		if !strings.HasPrefix(description.ServerVersion, "18") {
			t.Errorf("server_version = %q, want the pg18 image's version", description.ServerVersion)
		}
	})

	t.Run("search extensions the migrations create", func(t *testing.T) {
		versions := make(map[string]string)
		for _, ext := range description.Extensions {
			versions[ext.Name] = ext.Version
		}
		for _, name := range []string{"vector", "pg_trgm", "fuzzystrmatch"} {
			if versions[name] == "" {
				t.Errorf("extension %s missing or without a version; got %v", name, description.Extensions)
			}
		}
		if _, ok := versions["uuid-ossp"]; ok {
			t.Errorf("uuid-ossp recorded, but it plays no part in search")
		}
	})

	t.Run("hnsw index with its build parameters", func(t *testing.T) {
		index, ok := findIndex(description.Indexes, "idx_article_embedding")
		if !ok {
			t.Fatalf("idx_article_embedding not described; got %v", description.Indexes)
		}
		if index.Table != "article_embeddings" {
			t.Errorf("table = %q, want article_embeddings", index.Table)
		}
		if !strings.Contains(index.Definition, "USING hnsw") {
			t.Errorf("definition = %q, want an hnsw index", index.Definition)
		}
		declared := map[string]int{"m": storage.HNSWM, "ef_construction": storage.HNSWEfConstruction}
		for name, want := range declared {
			if got, ok := reloption(index.Options, name); !ok || got != want {
				t.Errorf("reloption %s = %d (present %v), want %d; reloptions = %v", name, got, ok, want, index.Options)
			}
		}
	})

	t.Run("articles indexes", func(t *testing.T) {
		if _, ok := findIndex(description.Indexes, "articles_pkey"); !ok {
			t.Errorf("articles primary key not described; got %v", description.Indexes)
		}
	})

	t.Run("connection settings the pool applied", func(t *testing.T) {
		for name, want := range specSettings {
			if got := description.Settings[name]; got != want {
				t.Errorf("setting %s = %q, want %q", name, got, want)
			}
		}
	})
}

func findIndex(indexes []IndexDefinition, name string) (IndexDefinition, bool) {
	for _, index := range indexes {
		if index.Name == name {
			return index, true
		}
	}
	return IndexDefinition{}, false
}
