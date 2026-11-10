package es

import (
	"context"
	"strings"
	"testing"

	"github.com/tadjov/tusker/internal/storage"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

func TestDescribeIndex_ReadsTheLiveIndex(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewESContainer(ctx, t)
	cfg := ClientConfig{Addresses: []string{container.Address}, IndexName: "articles_described_test"}
	if _, err := NewIndexer(ctx, cfg); err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}

	const alias = "news_described_test"
	client, err := newTypedClient(cfg)
	if err != nil {
		t.Fatalf("newTypedClient: %v", err)
	}
	if _, err := client.Indices.PutAlias(cfg.IndexName, alias).Do(ctx); err != nil {
		t.Fatalf("put alias: %v", err)
	}

	for _, name := range []string{cfg.IndexName, alias} {
		t.Run(name, func(t *testing.T) {
			description, err := DescribeIndex(ctx, ClientConfig{Addresses: cfg.Addresses, IndexName: name})
			if err != nil {
				t.Fatalf("DescribeIndex: %v", err)
			}

			if description.Version != "8.12.0" {
				t.Errorf("version = %q, want the container image's 8.12.0", description.Version)
			}
			if description.Index != cfg.IndexName {
				t.Errorf("index = %q, want the concrete index %q", description.Index, cfg.IndexName)
			}
			if !strings.Contains(string(description.Analysis), `"english_analyzer"`) {
				t.Errorf("analysis = %s, want it to define english_analyzer", description.Analysis)
			}

			title := description.Fields["title"]
			if title.Type != "text" || title.Analyzer != "english_analyzer" {
				t.Errorf("title = %+v, want a text field analysed by english_analyzer", title)
			}

			embedding := description.Fields["embedding"]
			if embedding.VectorIndex == nil {
				t.Fatalf("embedding = %+v, want its vector index options", embedding)
			}
			if embedding.VectorIndex.M != storage.HNSWM || embedding.VectorIndex.EfConstruction != storage.HNSWEfConstruction {
				t.Errorf("embedding index options = %+v, want m=%d ef_construction=%d",
					*embedding.VectorIndex, storage.HNSWM, storage.HNSWEfConstruction)
			}

			if _, ok := description.Fields["title.keyword"]; ok {
				t.Errorf("title.keyword recorded, but keyword fields are not analysed")
			}
		})
	}
}
