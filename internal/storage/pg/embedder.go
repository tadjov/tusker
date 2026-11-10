package pg

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage"
)

type Embedder struct {
	db *pgxpool.Pool
}

func NewEmbedder(pool *ConnectionPool) *Embedder {
	return &Embedder{db: pool.GetConn()}
}

func (e *Embedder) Save(ctx context.Context, article *embedding.Vec) (uuid.UUID, error) {
	vec := pgvector.NewVector(article.Embedding)
	cmd := `
		INSERT INTO article_embeddings (article_id, model_name, embedding)
		VALUES ($1, $2, $3)
		ON CONFLICT (article_id, model_name) DO UPDATE
		SET embedding = EXCLUDED.embedding
		RETURNING id
	`
	var id uuid.UUID
	err := e.db.QueryRow(
		ctx,
		cmd,
		article.ID,
		article.Model,
		vec,
	).Scan(&id)

	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to insert article embedding: %w", err)
	}

	return id, nil
}

// SaveBulk upserts a batch of embeddings. It COPYs into a temporary staging
// table, then inserts only rows whose article_id exists (orphans are skipped and
// logged) with ON CONFLICT upsert, making the operation re-runnable. DISTINCT ON
// collapses duplicate (article_id, model_name) rows within the batch, which would
// otherwise abort the upsert (Postgres forbids hitting a conflict row twice).
func (e *Embedder) SaveBulk(ctx context.Context, vecs []*embedding.Vec) (storage.EmbedWriteResult, error) {
	if len(vecs) == 0 {
		return storage.EmbedWriteResult{}, nil
	}

	tx, err := e.db.Begin(ctx)
	if err != nil {
		return storage.EmbedWriteResult{}, fmt.Errorf("failed to begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		CREATE TEMP TABLE _embed_stage (
			article_id uuid,
			model_name text,
			embedding  vector
		) ON COMMIT DROP
	`)
	if err != nil {
		return storage.EmbedWriteResult{}, fmt.Errorf("failed to create staging table: %w", err)
	}

	// Deduplicate before counting so Skipped means the same thing here as it
	// does on Elasticsearch: a vector whose article is absent. The staging
	// insert collapses repeats anyway; counting them as skipped would report a
	// missing article that is not missing.
	rows := make([][]any, 0, len(vecs))
	seen := make(map[[2]string]int, len(vecs))
	for _, v := range vecs {
		key := [2]string{v.ID.String(), v.Model}
		row := []any{v.ID, v.Model, pgvector.NewVector(v.Embedding)}
		if i, dup := seen[key]; dup {
			rows[i] = row
			continue
		}
		seen[key] = len(rows)
		rows = append(rows, row)
	}

	_, err = tx.CopyFrom(
		ctx,
		pgx.Identifier{"_embed_stage"},
		[]string{"article_id", "model_name", "embedding"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return storage.EmbedWriteResult{}, fmt.Errorf("failed to copy embeddings to staging: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO article_embeddings (article_id, model_name, embedding)
		SELECT DISTINCT ON (s.article_id, s.model_name)
			s.article_id, s.model_name, s.embedding
		FROM _embed_stage s
		JOIN articles a ON a.id = s.article_id
		ORDER BY s.article_id, s.model_name
		ON CONFLICT (article_id, model_name) DO UPDATE
		SET embedding = EXCLUDED.embedding
	`)
	if err != nil {
		return storage.EmbedWriteResult{}, fmt.Errorf("failed to upsert article embeddings: %w", err)
	}

	result := storage.EmbedWriteResult{
		Stored:  int(tag.RowsAffected()),
		Skipped: len(rows) - int(tag.RowsAffected()),
	}
	if result.Skipped > 0 {
		slog.Warn("skipped embeddings (orphan article or duplicate id)",
			"skipped", result.Skipped,
			"stored", result.Stored,
		)
	}

	if err := tx.Commit(ctx); err != nil {
		return storage.EmbedWriteResult{}, fmt.Errorf("failed to commit embeddings: %w", err)
	}

	return result, nil
}
