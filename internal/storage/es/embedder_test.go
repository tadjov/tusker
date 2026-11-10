package es

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

func newEmbedderTestEnv(t *testing.T) (ClientConfig, *Indexer, *Embedder) {
	t.Helper()

	container := pkgtesting.NewESContainer(context.Background(), t)
	cfg := ClientConfig{Addresses: []string{container.Address}, IndexName: "articles_embed_test"}

	indexer, err := NewIndexer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	embedder, err := NewEmbedder(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewEmbedder: %v", err)
	}
	return cfg, indexer, embedder
}

func vec(dim int, fill float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = fill
	}
	return v
}

func docEmbedding(t *testing.T, e *Embedder, id uuid.UUID) ([]float32, bool) {
	t.Helper()
	res, err := e.client.Get(e.indexName, id.String()).Do(context.Background())
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	if !res.Found {
		return nil, false
	}
	var src struct {
		Embedding []float32 `json:"embedding"`
		Model     string    `json:"embedding_model"`
	}
	if err := json.Unmarshal(res.Source_, &src); err != nil {
		t.Fatalf("unmarshal source: %v", err)
	}
	return src.Embedding, true
}

func refresh(t *testing.T, e *Embedder) {
	t.Helper()
	if _, err := e.client.Indices.Refresh().Index(e.indexName).Do(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
}

func TestEmbedder_SaveBulk_UpdatesDocAndSkipsOrphans(t *testing.T) {
	ctx := context.Background()
	_, indexer, embedder := newEmbedderTestEnv(t)

	a1 := uuid.New()
	if _, err := indexer.Save(ctx, document.Article{ID: a1, Title: "first", Language: language.English}); err != nil {
		t.Fatalf("index article: %v", err)
	}
	refresh(t, embedder)

	const model = "qwen3-embedding:0.6b"
	orphan := uuid.New()
	batch := []*embedding.Vec{
		{ID: a1, Model: model, Embedding: vec(EmbeddingDims, 0.1)},
		{ID: orphan, Model: model, Embedding: vec(EmbeddingDims, 0.2)},
	}

	res, err := embedder.SaveBulk(ctx, batch)
	if err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}
	if res.Stored != 1 || res.Skipped != 1 {
		t.Errorf("stored=%d skipped=%d, want 1 and 1: the orphan must be reported, not just logged",
			res.Stored, res.Skipped)
	}
	refresh(t, embedder)

	emb, found := docEmbedding(t, embedder, a1)
	if !found {
		t.Fatal("article doc disappeared")
	}
	if len(emb) != EmbeddingDims {
		t.Fatalf("embedding dim = %d, want %d", len(emb), EmbeddingDims)
	}

	if _, found := docEmbedding(t, embedder, orphan); found {
		t.Error("orphan id should not have created a document")
	}

	// Idempotent re-run with a changed vector → no error, value updated.
	batch[0].Embedding = vec(EmbeddingDims, 0.9)
	if _, err := embedder.SaveBulk(ctx, batch); err != nil {
		t.Fatalf("SaveBulk re-run: %v", err)
	}
	refresh(t, embedder)

	emb, _ = docEmbedding(t, embedder, a1)
	if len(emb) == 0 || emb[0] != 0.9 {
		t.Errorf("expected upserted embedding[0]=0.9, got %v", emb[:1])
	}
}

// The document exists either way; only the embedding may move the count.
func TestIndexer_CountEmbedded(t *testing.T) {
	ctx := context.Background()
	_, indexer, embedder := newEmbedderTestEnv(t)

	id := uuid.New()
	if _, err := indexer.Save(ctx, document.Article{ID: id, Title: "first", Language: language.English}); err != nil {
		t.Fatalf("index article: %v", err)
	}
	refresh(t, embedder)

	if n, err := indexer.CountEmbedded(ctx); err != nil || n != 0 {
		t.Fatalf("CountEmbedded before embedding = %d, %v; want 0, nil", n, err)
	}

	if _, err := embedder.SaveBulk(ctx, []*embedding.Vec{
		{ID: id, Model: "qwen3-embedding:0.6b", Embedding: vec(EmbeddingDims, 0.1)},
	}); err != nil {
		t.Fatalf("save embedding: %v", err)
	}
	refresh(t, embedder)

	if n, err := indexer.CountEmbedded(ctx); err != nil || n != 1 {
		t.Fatalf("CountEmbedded after embedding = %d, %v; want 1, nil", n, err)
	}
}
