package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/pg"
)

type PgExecutor struct {
	name     string
	executor storage.RawExecutor
	pool     *pg.ConnectionPool
}

func NewPgExecutor(name string, pool *pg.ConnectionPool) *PgExecutor {
	return &PgExecutor{
		name:     name,
		executor: pg.NewRawExecutor(pool),
		pool:     pool,
	}
}

func (e *PgExecutor) Execute(ctx context.Context, req Request) (*Execution, error) {
	start := time.Now()

	result, err := e.executor.Exec(ctx, req.Query, req.Args, nil)
	if err != nil {
		return nil, fmt.Errorf("pg exec: %w", err)
	}

	latency := time.Since(start)

	ids := make([]uuid.UUID, 0, len(result.Hits))
	for _, hit := range result.Hits {
		id, err := extractUUID(hit["id"])
		if err != nil {
			return nil, fmt.Errorf("pg extract id: %w", err)
		}
		ids = append(ids, id)
	}

	return &Execution{
		RankedDocIDs: ids,
		Latency:      latency,
	}, nil
}

func (e *PgExecutor) Name() string { return e.name }
func (e *PgExecutor) Close() error { return nil }

// Validate runs EXPLAIN on the query. This catches syntax errors and missing
// columns/tables/operators without scanning data. ParadeDB's pdb.* functions
// also surface here, so it's a real correctness check for those queries too.
func (e *PgExecutor) Validate(ctx context.Context, req Request) error {
	if _, err := e.executor.Exec(ctx, "EXPLAIN "+req.Query, req.Args, nil); err != nil {
		return fmt.Errorf("pg explain: %w", err)
	}
	return nil
}

func (e *PgExecutor) CorpusCount(ctx context.Context) (int64, error) {
	res, err := e.executor.Exec(ctx, "SELECT count(*) AS n FROM articles", nil, nil)
	if err != nil {
		return 0, fmt.Errorf("pg corpus count: %w", err)
	}
	if len(res.Hits) == 0 {
		return 0, fmt.Errorf("pg corpus count: no rows")
	}
	switch v := res.Hits[0]["n"].(type) {
	case int64:
		return v, nil
	case int32:
		return int64(v), nil
	case int:
		return int64(v), nil
	default:
		return 0, fmt.Errorf("pg corpus count: unsupported type %T", v)
	}
}

func extractUUID(val interface{}) (uuid.UUID, error) {
	switch v := val.(type) {
	case [16]byte:
		return uuid.UUID(v), nil
	case uuid.UUID:
		return v, nil
	case string:
		return uuid.Parse(v)
	default:
		return uuid.Nil, fmt.Errorf("unsupported id type %T", val)
	}
}
