package es

import (
	"context"
	"errors"
	"testing"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/densevectorsimilarity"
	"github.com/tadjov/tusker/internal/storage"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

func TestIndexBuilder_DenseVectorDeclaresHNSWBuildParams(t *testing.T) {
	p := NewIndexBuilder().denseVectorProperty()

	if p.IndexOptions == nil {
		t.Fatal("embedding field left index_options unset, so Elasticsearch builds at its own default")
	}
	if got := p.IndexOptions.Type.String(); got != "hnsw" {
		t.Errorf("index_options.type = %q, want hnsw", got)
	}
	if p.IndexOptions.M == nil || *p.IndexOptions.M != storage.HNSWM {
		t.Errorf("index_options.m = %v, want %d", p.IndexOptions.M, storage.HNSWM)
	}
	if p.IndexOptions.EfConstruction == nil || *p.IndexOptions.EfConstruction != storage.HNSWEfConstruction {
		t.Errorf("index_options.ef_construction = %v, want %d", p.IndexOptions.EfConstruction, storage.HNSWEfConstruction)
	}
}

func TestEnsureIndex_MappingReportsHNSWBuildParams(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewESContainer(ctx, t)
	cfg := ClientConfig{Addresses: []string{container.Address}, IndexName: "articles_hnsw_test"}

	if _, err := NewIndexer(ctx, cfg); err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}

	client, err := newTypedClient(cfg)
	if err != nil {
		t.Fatalf("newTypedClient: %v", err)
	}
	assertDeclaredBuildParams(t, client, cfg.IndexName)
}

func TestEmbedder_RejectsAnIndexBuiltAtOtherBuildParams(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewESContainer(ctx, t)
	cfg := ClientConfig{Addresses: []string{container.Address}, IndexName: "articles_hnsw_legacy_test"}

	client, err := newTypedClient(cfg)
	if err != nil {
		t.Fatalf("newTypedClient: %v", err)
	}
	createIndexWithDefaultedVectorField(t, client, cfg.IndexName)

	_, err = NewEmbedder(ctx, cfg)
	if err == nil {
		t.Fatal("NewEmbedder accepted an index built at the elasticsearch default parameters")
	}
	var mismatch *VectorIndexMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error is %v, want a *VectorIndexMismatchError naming the rebuild", err)
	}
	if mismatch.Index != cfg.IndexName {
		t.Errorf("mismatch reports index %q, want %q", mismatch.Index, cfg.IndexName)
	}
}

// createIndexWithDefaultedVectorField shapes the field the way it was before the
// build parameters were declared: indexed and cosine, index_options unset.
func createIndexWithDefaultedVectorField(t *testing.T, client *elasticsearch.TypedClient, index string) {
	t.Helper()

	dims := EmbeddingDims
	indexed := true
	sim := densevectorsimilarity.Cosine
	vector := types.NewDenseVectorProperty()
	vector.Dims = &dims
	vector.Index = &indexed
	vector.Similarity = &sim

	mappings := types.TypeMapping{
		Properties: map[string]types.Property{
			"id":        types.NewKeywordProperty(),
			"embedding": vector,
		},
	}
	if _, err := client.Indices.Create(index).Mappings(&mappings).Do(context.Background()); err != nil {
		t.Fatalf("create index with a defaulted vector field: %v", err)
	}
}

func assertDeclaredBuildParams(t *testing.T, client *elasticsearch.TypedClient, index string) {
	t.Helper()

	res, err := client.Indices.GetMapping().Index(index).Do(context.Background())
	if err != nil {
		t.Fatalf("get mapping: %v", err)
	}
	record, ok := res[index]
	if !ok {
		t.Fatalf("no mapping returned for index %q", index)
	}
	vector, ok := record.Mappings.Properties["embedding"].(*types.DenseVectorProperty)
	if !ok {
		t.Fatalf("embedding property is %T, want *types.DenseVectorProperty", record.Mappings.Properties["embedding"])
	}
	if vector.IndexOptions == nil {
		t.Fatal("elasticsearch reports no index_options for the embedding field")
	}
	if vector.IndexOptions.M == nil || *vector.IndexOptions.M != storage.HNSWM {
		t.Errorf("index_options.m = %v, want %d", vector.IndexOptions.M, storage.HNSWM)
	}
	if vector.IndexOptions.EfConstruction == nil || *vector.IndexOptions.EfConstruction != storage.HNSWEfConstruction {
		t.Errorf("index_options.ef_construction = %v, want %d", vector.IndexOptions.EfConstruction, storage.HNSWEfConstruction)
	}
}
