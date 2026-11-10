package factory

import (
	"context"
	"fmt"

	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/es"
	"github.com/tadjov/tusker/internal/storage/in_mem"
	"github.com/tadjov/tusker/internal/storage/pg"
	"github.com/tadjov/tusker/internal/storage/pg/native"
	"github.com/tadjov/tusker/pkg/server"
)

// TODO: take the pool as a parameter in the indexer and reader constructors too, as the searchers do, instead of opening one each

// NewIndexer creates a new storage.Indexer based on the storage type
func NewIndexer(ctx context.Context, cfg StorageConfig) (storage.Indexer, error) {
	switch cfg.Type {
	case storage.PG:
		pgConfig := *cfg.Pg

		pool, err := pg.NewConnectionPool(ctx, pgConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create PostgreSQL connection pool: %w", err)
		}

		return pg.NewIndexer(pool)

	case storage.ES:
		esConfig := *cfg.Es

		return es.NewIndexer(ctx, esConfig)

	case storage.Solr:
		return nil, fmt.Errorf("solr storer not yet implemented")

	case storage.InMem:
		return in_mem.NewInMemIndexer(), nil

	default:
		return nil, fmt.Errorf(string(storage.ErrUnsupportedStorer), cfg.Type)
	}
}

func NewEmbedderIndexer(ctx context.Context, cfg StorageConfig) (storage.EmbedIndexer, error) {
	switch cfg.Type {
	case storage.PG:
		pgConfig := pg.PoolConfig{
			ConnStr:          cfg.Pg.ConnStr,
			RegisterVecTypes: true,
		}

		pool, err := pg.NewConnectionPool(ctx, pgConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create PostgreSQL connection pool: %w", err)
		}

		return pg.NewEmbedder(pool), nil

	case storage.ES:
		if cfg.Es == nil {
			return nil, fmt.Errorf("elasticsearch config is not set")
		}
		return es.NewEmbedder(ctx, *cfg.Es)
	}
	return nil, fmt.Errorf(string(storage.ErrUnsupportedStorer), cfg.Type)
}

// SearcherConfig selects the backend searchers read from. Every PG searcher
// runs on Pool and every ES searcher on EsClient, so each backend's searchers
// share one set of connections; the caller owns and closes the pool.
// Embedder is required for semantic and hybrid searchers.
type SearcherConfig struct {
	Type     storage.Type
	Pool     *pg.ConnectionPool
	EsClient *es.Client
	Embedder *embedding.Embedder
}

// NewSearcher creates a new storage.FtsSearcher based on the storage type
func NewSearcher(cfg SearcherConfig) (storage.FtsSearcher, error) {
	switch cfg.Type {
	case storage.PG:
		if cfg.Pool == nil {
			return nil, fmt.Errorf("postgres pool is not set")
		}
		return native.NewReader(cfg.Pool)

	case storage.ES:
		if cfg.EsClient == nil {
			return nil, fmt.Errorf("elasticsearch client is not set")
		}
		return es.NewSearcher(cfg.EsClient), nil

	case storage.Solr:
		return nil, fmt.Errorf("solr reader not yet implemented")

	case storage.InMem:
		// TODO: Implement InMem when needed
		return nil, fmt.Errorf("inmem reader not yet implemented")

	default:
		return nil, fmt.Errorf(string(storage.ErrUnsupportedStorer), cfg.Type)
	}
}

func NewReader(ctx context.Context, cfg StorageConfig) (storage.Reader, error) {
	if cfg.Type != storage.PG {
		return nil, fmt.Errorf("reader not supported for storage type %s", cfg.Type)
	}

	pool, err := pg.NewConnectionPool(ctx, *cfg.Pg)
	if err != nil {
		return nil, fmt.Errorf("failed to create PostgreSQL connection pool: %w", err)
	}

	return pg.NewArticleReader(pool), nil
}

func NewSemanticSearcher(cfg SearcherConfig) (storage.SemanticSearcher, error) {
	if cfg.Embedder == nil {
		return nil, fmt.Errorf("query embedder is not set")
	}
	switch cfg.Type {
	case storage.PG:
		if cfg.Pool == nil {
			return nil, fmt.Errorf("postgres pool is not set")
		}
		return pg.NewSemanticSearcher(cfg.Embedder, cfg.Pool), nil

	case storage.ES:
		if cfg.EsClient == nil {
			return nil, fmt.Errorf("elasticsearch client is not set")
		}
		return es.NewSemanticSearcher(cfg.EsClient, cfg.Embedder, cfg.Embedder.Model()), nil

	case storage.Solr:
		return nil, fmt.Errorf("solr semantic searcher not yet implemented")

	case storage.InMem:
		return nil, fmt.Errorf("inmem semantic searcher not yet implemented")

	default:
		return nil, fmt.Errorf(string(storage.ErrUnsupportedStorer), cfg.Type)
	}
}

func NewHybridSearcher(cfg SearcherConfig) (storage.HybridSearcher, error) {
	if cfg.Embedder == nil {
		return nil, fmt.Errorf("query embedder is not set")
	}
	switch cfg.Type {
	case storage.PG:
		if cfg.Pool == nil {
			return nil, fmt.Errorf("postgres pool is not set")
		}
		return pg.NewHybridSearcher(cfg.Embedder, cfg.Pool), nil

	case storage.ES:
		if cfg.EsClient == nil {
			return nil, fmt.Errorf("elasticsearch client is not set")
		}
		return es.NewHybridSearcher(cfg.EsClient, cfg.Embedder, cfg.Embedder.Model()), nil

	case storage.Solr:
		return nil, fmt.Errorf("solr hybrid searcher not yet implemented")

	case storage.InMem:
		return nil, fmt.Errorf("inmem hybrid searcher not yet implemented")

	default:
		return nil, fmt.Errorf(string(storage.ErrUnsupportedStorer), cfg.Type)
	}
}

// NewHealthChecker reports the health of the backend the searchers in cfg read from.
func NewHealthChecker(cfg SearcherConfig) (server.HealthChecker, error) {
	switch cfg.Type {
	case storage.PG:
		if cfg.Pool == nil {
			return nil, fmt.Errorf("postgres pool is not set")
		}
		return pg.NewHealthChecker(cfg.Pool), nil

	case storage.ES:
		if cfg.EsClient == nil {
			return nil, fmt.Errorf("elasticsearch client is not set")
		}
		return es.NewHealthChecker(cfg.EsClient), nil

	default:
		return nil, fmt.Errorf("no health check for storage type %s", cfg.Type)
	}
}
