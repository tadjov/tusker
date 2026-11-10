package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tadjov/tusker/internal/storage"
)

type RawExecutor struct {
	db *pgxpool.Pool
}

func NewRawExecutor(pool *ConnectionPool) *RawExecutor {
	return &RawExecutor{db: pool.GetConn()}
}

func (e *RawExecutor) Exec(
	ctx context.Context,
	query string,
	params []interface{},
	opts *storage.ExecOptions) (*storage.ExecuteResult, error) {
	queryCtx, cancel := e.newQueryCtx(ctx, opts)
	defer cancel()

	rows, err := e.db.Query(queryCtx, query, params...)

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}

		rowMap := make(map[string]interface{})
		fieldDescriptions := rows.FieldDescriptions()
		for i, fd := range fieldDescriptions {
			rowMap[fd.Name] = values[i]
		}
		results = append(results, rowMap)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &storage.ExecuteResult{
		RowCount: len(results),
		Hits:     results,
	}, nil
}

func (e *RawExecutor) newQueryCtx(ctx context.Context, opts *storage.ExecOptions) (context.Context, context.CancelFunc) {
	if opts != nil && opts.TimeoutSeconds > 0 {
		queryCtx, cancel := context.WithTimeout(ctx, time.Duration(opts.TimeoutSeconds)*time.Second)
		return queryCtx, cancel
	}
	return ctx, func() {
		// no-op
	}
}

var _ storage.RawExecutor = (*RawExecutor)(nil)
