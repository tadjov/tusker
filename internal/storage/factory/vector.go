package factory

import (
	"context"
	"fmt"

	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/es"
	"github.com/tadjov/tusker/internal/storage/pg"
)

// VectorStoreConfig selects and configures a storage.VectorStore. Postgres
// takes precedence: when a PG connection is available the PG store is used;
// Elasticsearch is the fallback when only an ES backend is configured.
type VectorStoreConfig struct {
	PgConnStr string
	Es        *es.ClientConfig

	// EmbeddingClient embeds query text (the document vectors come from the
	// store).
	EmbeddingClient embedding.Client
	// Model defaults to embedding.DefaultModel.
	Model string
	// Dim defaults to embedding.DefaultDim.
	Dim int
}

// NewVectorStore builds a VectorStore, preferring Postgres over Elasticsearch.
func NewVectorStore(ctx context.Context, cfg VectorStoreConfig) (storage.VectorStore, error) {
	if cfg.PgConnStr == "" && cfg.Es == nil {
		return nil, fmt.Errorf("vector store: no Postgres or Elasticsearch backend configured")
	}
	if cfg.EmbeddingClient == nil {
		return nil, fmt.Errorf("vector store: embedding client is required for query embedding")
	}
	var opts []embedding.EmbedderOption
	if cfg.Model != "" {
		opts = append(opts, embedding.WithExecutorModel(cfg.Model))
	}
	if cfg.Dim != 0 {
		opts = append(opts, embedding.WithDim(cfg.Dim))
	}
	embedder := embedding.NewEmbedder(cfg.EmbeddingClient, opts...)

	if cfg.PgConnStr == "" {
		return es.NewVectorStore(*cfg.Es, embedder, embedder.Model())
	}
	pool, err := pg.NewConnectionPool(ctx, pg.PoolConfig{
		ConnStr:          cfg.PgConnStr,
		RegisterVecTypes: true,
	})
	if err != nil {
		return nil, fmt.Errorf("vector store: create PostgreSQL pool: %w", err)
	}
	return pg.NewVectorStore(embedder, pool, embedder.Model()), nil
}
