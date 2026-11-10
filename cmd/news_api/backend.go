package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/es"
	"github.com/tadjov/tusker/internal/storage/factory"
	"github.com/tadjov/tusker/internal/storage/pg"
	pkgserver "github.com/tadjov/tusker/pkg/server"
)

// searchBackend is everything the API serves from. semantic and hybrid are nil
// when embeddings are disabled; close releases the shared Postgres pool or
// Elasticsearch client.
type searchBackend struct {
	fts      storage.FtsSearcher
	semantic storage.SemanticSearcher
	hybrid   storage.HybridSearcher
	health   pkgserver.HealthChecker
	close    func()
}

func openSearchBackend(ctx context.Context, cfg *NewsSearchConfig) (*searchBackend, error) {
	searcherCfg := factory.SearcherConfig{Type: cfg.StorageConfig.Type}
	backend := &searchBackend{close: func() {}}

	switch cfg.StorageConfig.Type {
	case storage.PG:
		poolCfg := *cfg.StorageConfig.Pg
		poolCfg.RegisterVecTypes = cfg.EmbeddingConfig.Enabled
		pool, err := pg.NewConnectionPool(ctx, poolCfg)
		if err != nil {
			return nil, fmt.Errorf("create PostgreSQL connection pool: %w", err)
		}
		searcherCfg.Pool = pool
		backend.close = pool.Close
	case storage.ES:
		client, err := es.NewClient(*cfg.StorageConfig.Es)
		if err != nil {
			return nil, fmt.Errorf("create Elasticsearch client: %w", err)
		}
		searcherCfg.EsClient = client
		backend.close = client.Close
	}

	if err := backend.wire(searcherCfg, cfg.EmbeddingConfig); err != nil {
		backend.close()
		return nil, err
	}
	return backend, nil
}

func (b *searchBackend) wire(searcherCfg factory.SearcherConfig, embeddingCfg embedding.Config) error {
	var err error
	b.health, err = factory.NewHealthChecker(searcherCfg)
	if err != nil {
		return fmt.Errorf("create health checker: %w", err)
	}

	b.fts, err = factory.NewSearcher(searcherCfg)
	if err != nil {
		return fmt.Errorf("create storage searcher: %w", err)
	}

	if !embeddingCfg.Enabled {
		return nil
	}

	embedClient, err := embedding.NewOllamaClient(embeddingCfg.BaseURL)
	if err != nil {
		return fmt.Errorf("create embedding client: %w", err)
	}
	searcherCfg.Embedder = newQueryEmbedder(embedClient, embeddingCfg)
	slog.Info("Embedding queries", "model", searcherCfg.Embedder.Model())

	b.semantic, err = factory.NewSemanticSearcher(searcherCfg)
	if err != nil {
		return fmt.Errorf("create semantic searcher: %w", err)
	}

	b.hybrid, err = factory.NewHybridSearcher(searcherCfg)
	if err != nil {
		slog.Warn("Hybrid search disabled: failed to create hybrid searcher", "error", err)
	}

	return nil
}
