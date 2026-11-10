package pg

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tadjov/tusker/internal/api/dto"
	"github.com/tadjov/tusker/internal/types/document"
)

// ArticleColumns must stay in scanArticle's scan order.
var ArticleColumns = []string{
	"id", "url", "title", "description", "content", "published_at",
	"language", "publisher_domain", "metadata",
}

// ArticleColumnList renders the projection, optionally table-qualified.
func ArticleColumnList(alias string) string {
	if alias == "" {
		return strings.Join(ArticleColumns, ", ")
	}
	qualified := make([]string, len(ArticleColumns))
	for i, c := range ArticleColumns {
		qualified[i] = alias + "." + c
	}
	return strings.Join(qualified, ", ")
}

// scanArticle scans trailing targets after the article columns.
func scanArticle(rows pgx.Rows, trailing ...any) (document.Article, error) {
	var article document.Article
	var publishedAt *time.Time
	var metadataJSON []byte

	targets := append([]any{
		&article.ID,
		&article.URL,
		&article.Title,
		&article.Description,
		&article.Content,
		&publishedAt,
		&article.Language,
		&article.PublisherDomain,
		&metadataJSON,
	}, trailing...)

	if err := rows.Scan(targets...); err != nil {
		return document.Article{}, fmt.Errorf("failed to scan article: %w", err)
	}

	if publishedAt != nil {
		article.PublishedAt = *publishedAt
	}
	if err := json.Unmarshal(metadataJSON, &article.Metadata); err != nil {
		return document.Article{}, fmt.Errorf("failed to unmarshal metadata of article %s: %w", article.ID, err)
	}

	return article, nil
}

func ScanArticle(rows pgx.Rows, trailing ...any) (*dto.Article, error) {
	article, err := scanArticle(rows, trailing...)
	if err != nil {
		return nil, err
	}
	result := dto.NewArticle(article)
	return &result, nil
}

func MapToArticle(rows pgx.Rows) (*dto.Article, float32, error) {
	var distance float32
	article, err := ScanArticle(rows, &distance)
	if err != nil {
		return nil, 0, err
	}
	return article, distance, nil
}

// nullableTime maps the zero time to SQL NULL, so an article with no
// publication date sorts as unknown rather than as year 1.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
