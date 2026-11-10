package engine

import (
	"context"
	"fmt"

	"github.com/tadjov/tusker/internal/bench/spec"
	"github.com/tadjov/tusker/internal/storage/pg"
)

func CreateFromSpec(ctx context.Context, engines map[string]spec.Engine) (map[string]Executor, func(), error) {
	executors := make(map[string]Executor, len(engines))
	var cleanups []func()

	cleanup := func() {
		for _, c := range cleanups {
			c()
		}
	}

	for name, eng := range engines {
		switch eng.Type {
		case spec.EnginePostgres:
			pool, err := pg.NewConnectionPool(ctx, pg.PoolConfig{
				ConnStr:            eng.Connection,
				ConnectionSettings: eng.ConnectionSettings,
			})
			if err != nil {
				cleanup()
				return nil, nil, fmt.Errorf("create pg pool for %q: %w", name, err)
			}
			cleanups = append(cleanups, pool.Close)
			executors[name] = NewPgExecutor(name, pool)

		case spec.EngineElasticsearch:
			index := eng.Index
			if index == "" {
				index = "news"
			}
			executors[name] = NewEsExecutor(name, eng.Connection, index)

		case spec.EngineAPI:
			executors[name] = NewAPIExecutor(name, eng.Connection)

		default:
			cleanup()
			return nil, nil, fmt.Errorf("unsupported engine type %q for %q", eng.Type, name)
		}
	}

	return executors, cleanup, nil
}
