package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
)

type Indexer struct {
	db *pgxpool.Pool
}

func NewIndexer(pool *ConnectionPool) (*Indexer, error) {

	return &Indexer{db: pool.conn}, nil
}

func (s *Indexer) Save(ctx context.Context, article document.Article) (uuid.UUID, error) {
	row, err := insertRow(article)
	if err != nil {
		return uuid.UUID{}, err
	}

	placeholders := make([]string, len(ArticleColumns))
	for i := range placeholders {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	cmd := fmt.Sprintf("INSERT INTO articles (%s) VALUES (%s) RETURNING id",
		ArticleColumnList(""), strings.Join(placeholders, ", "))

	var id uuid.UUID
	if err := s.db.QueryRow(ctx, cmd, row...).Scan(&id); err != nil {
		return uuid.UUID{}, fmt.Errorf("failed to insert article: %w", err)
	}

	return id, nil
}

func (s *Indexer) SaveBulk(ctx context.Context, articles []document.Article) error {
	rows := make([][]any, len(articles))
	for i, a := range articles {
		row, err := insertRow(a)
		if err != nil {
			return fmt.Errorf("article %d: %w", i, err)
		}
		rows[i] = row
	}

	_, err := s.db.CopyFrom(
		ctx,
		pgx.Identifier{"articles"},
		ArticleColumns,
		pgx.CopyFromRows(rows),
	)

	if err != nil {
		return fmt.Errorf("failed to bulk insert articles: %w", err)
	}
	return nil
}

func insertRow(a document.Article) ([]any, error) {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	if _, err := language.Parse(string(a.Language)); err != nil {
		return nil, fmt.Errorf("article %s: %w", a.ID, err)
	}

	metadataJSON, err := json.Marshal(a.Metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal metadata of article %s: %w", a.ID, err)
	}

	return []any{
		a.ID,
		a.URL,
		a.Title,
		a.Description,
		a.Content,
		nullableTime(a.PublishedAt),
		string(a.Language),
		a.PublisherDomain,
		metadataJSON,
	}, nil
}
