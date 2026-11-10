package pg

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tadjov/tusker/internal/types/document"
)

type ArticleReader struct {
	db *pgxpool.Pool
}

func NewArticleReader(pool *ConnectionPool) *ArticleReader {
	return &ArticleReader{db: pool.GetConn()}
}

func (r *ArticleReader) GetByIDs(ctx context.Context, ids []uuid.UUID) ([]document.Article, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	pgUUIDs := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		pgUUIDs[i] = pgtype.UUID{Bytes: id, Valid: true}
	}

	q := fmt.Sprintf("SELECT %s FROM articles WHERE id = ANY($1)", ArticleColumnList(""))
	rows, err := r.db.Query(ctx, q, pgUUIDs)
	if err != nil {
		return nil, fmt.Errorf("query articles by ids: %w", err)
	}
	defer rows.Close()

	var articles []document.Article
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			return nil, err
		}
		articles = append(articles, a)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate article rows: %w", err)
	}

	return articles, nil
}
