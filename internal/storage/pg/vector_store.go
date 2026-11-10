package pg

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/tadjov/tusker/internal/embedding"
)

// VectorStore reads document embeddings from article_embeddings and embeds
// queries with the same model, so query and document vectors are always
// comparable. It is the Postgres implementation of storage.VectorStore.
type VectorStore struct {
	embedder *embedding.Embedder
	db       *pgxpool.Pool
	model    string
}

func NewVectorStore(embedder *embedding.Embedder, pool *ConnectionPool, model string) *VectorStore {
	return &VectorStore{
		embedder: embedder,
		db:       pool.GetConn(),
		model:    model,
	}
}

func (s *VectorStore) QueryVector(ctx context.Context, text string) ([]float32, error) {
	vec, err := s.embedder.EmbedQuery(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	return vec.Embedding, nil
}

func (s *VectorStore) DocVectors(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]float32, error) {
	out := make(map[uuid.UUID][]float32, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	const cmd = `
		SELECT article_id, embedding
		FROM article_embeddings
		WHERE article_id = ANY($1) AND model_name = $2
	`
	rows, err := s.db.Query(ctx, cmd, ids, s.model)
	if err != nil {
		return nil, fmt.Errorf("query article embeddings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id uuid.UUID
		var vec pgvector.Vector
		if err := rows.Scan(&id, &vec); err != nil {
			return nil, fmt.Errorf("scan article embedding: %w", err)
		}
		out[id] = vec.Slice()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate article embeddings: %w", err)
	}
	return out, nil
}

func (s *VectorStore) Model() string {
	return s.model
}
