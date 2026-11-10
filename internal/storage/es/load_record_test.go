package es

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

// loadRecord matches what the Elasticsearch loader stores under _meta.load.
const loadRecord = `{"engine": "elasticsearch", "engineVersion": "8.12.0",
  "startedAt": "2026-10-01T11:00:00Z", "finishedAt": "2026-10-01T11:20:00Z",
  "canonical": {"name": "cc-news", "version": "article-v2-601d5476", "resourceHash": "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
  "embeddings": {"name": "cc-news-embeddings", "version": "lead-v1-1024-938b2254", "resourceHash": "sha256:3333333333333333333333333333333333333333333333333333333333333333"},
  "modelTag": "qwen3-embedding:0.6b@1024", "articles": 707910, "vectors": 707910, "batchSize": 5000,
  "stages": [{"name": "bulk", "seconds": 600}, {"name": "force_merge", "seconds": 60}],
  "settings": {"number_of_replicas": "0"}}`

func TestDescribeIndex_ReadsTheLoadRecordFromTheIndexMeta(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewESContainer(ctx, t)
	cfg := ClientConfig{Addresses: []string{container.Address}, IndexName: "articles_load_record_test"}
	if _, err := NewIndexer(ctx, cfg); err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}

	t.Run("an index nothing recorded a load on", func(t *testing.T) {
		description, err := DescribeIndex(ctx, cfg)
		if err != nil {
			t.Fatalf("DescribeIndex: %v", err)
		}
		if description.Load != nil {
			t.Errorf("load = %+v, want none", description.Load)
		}
	})

	t.Run("an index carrying _meta.load", func(t *testing.T) {
		client, err := newTypedClient(cfg)
		if err != nil {
			t.Fatalf("newTypedClient: %v", err)
		}
		if _, err := client.Indices.PutMapping(cfg.IndexName).
			Meta_(types.Metadata{"load": json.RawMessage(loadRecord)}).Do(ctx); err != nil {
			t.Fatalf("put _meta.load: %v", err)
		}

		description, err := DescribeIndex(ctx, cfg)
		if err != nil {
			t.Fatalf("DescribeIndex: %v", err)
		}

		load := description.Load
		if load == nil {
			t.Fatal("load not described")
		}
		if load.Engine != "elasticsearch" || load.Canonical.Version != "article-v2-601d5476" {
			t.Errorf("engine, canonical = %q, %+v, want elasticsearch at article-v2-601d5476", load.Engine, load.Canonical)
		}
		if load.Embeddings == nil || load.Embeddings.Version != "lead-v1-1024-938b2254" {
			t.Errorf("embeddings = %+v, want lead-v1-1024-938b2254", load.Embeddings)
		}
		if load.ModelTag != "qwen3-embedding:0.6b@1024" {
			t.Errorf("model tag = %q, want qwen3-embedding:0.6b@1024", load.ModelTag)
		}
	})
}
