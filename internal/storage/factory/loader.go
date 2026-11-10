package factory

import (
	"context"
	"errors"
	"fmt"

	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/es"
	"github.com/tadjov/tusker/internal/storage/pg"
)

type LoaderConfig struct {
	Engine storage.Engine
	Pg     *pg.PoolConfig
	Es     *es.ClientConfig
}

// NewCorpusLoader returns the loader and a func that closes its connections.
func NewCorpusLoader(ctx context.Context, cfg LoaderConfig) (storage.CorpusLoader, func(), error) {
	switch cfg.Engine.Type() {
	case storage.PG:
		if cfg.Pg == nil {
			return nil, nil, errors.New("postgres config is not set")
		}
		// The loader registers vector types after its migrations create them.
		pool, err := pg.NewConnectionPool(ctx, pg.PoolConfig{ConnStr: cfg.Pg.ConnStr})
		if err != nil {
			return nil, nil, fmt.Errorf("connect to %s: %w", cfg.Engine, err)
		}
		loader, err := pg.NewCorpusLoader(pool, cfg.Engine)
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		return loader, pool.Close, nil
	case storage.ES:
		if cfg.Es == nil {
			return nil, nil, errors.New("elasticsearch config is not set")
		}
		client, err := es.NewClient(*cfg.Es)
		if err != nil {
			return nil, nil, fmt.Errorf("connect to %s: %w", cfg.Engine, err)
		}
		return es.NewCorpusLoader(client), client.Close, nil
	default:
		return nil, nil, fmt.Errorf("no corpus loader for engine %q", cfg.Engine)
	}
}
