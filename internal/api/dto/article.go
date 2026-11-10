package dto

import (
	"time"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/types/document"
)

type Article struct {
	ID              uuid.UUID  `json:"id"`
	URL             string     `json:"url" swaggertype:"string" format:"string"`
	Title           string     `json:"title"`
	Description     string     `json:"description,omitempty"`
	Content         string     `json:"content"`
	PublishedAt     *time.Time `json:"publishedAt,omitempty"`
	Language        string     `json:"language"`
	PublisherDomain string     `json:"publisherDomain,omitempty"`
	Metadata        Metadata   `json:"metadata,omitzero"`
}

type Metadata struct {
	ExternalID    string     `json:"externalId,omitempty"`
	PublisherName string     `json:"publisherName,omitempty"`
	Authors       []string   `json:"authors,omitempty"`
	Section       string     `json:"section,omitempty"`
	Keywords      []string   `json:"keywords,omitempty"`
	Image         string     `json:"image,omitempty"`
	DateModified  *time.Time `json:"dateModified,omitempty"`

	// Extra carries dataset-specific fields that have no schema field.
	Extra map[string]any `json:"extra,omitempty"`
}

func NewArticle(a document.Article) Article {
	return Article{
		ID:              a.ID,
		URL:             a.URL,
		Title:           a.Title,
		Description:     a.Description,
		Content:         a.Content,
		PublishedAt:     optionalTime(a.PublishedAt),
		Language:        string(a.Language),
		PublisherDomain: a.PublisherDomain,
		Metadata: Metadata{
			ExternalID:    a.Metadata.ExternalID,
			PublisherName: a.Metadata.PublisherName,
			Authors:       a.Metadata.Authors,
			Section:       a.Metadata.Section,
			Keywords:      a.Metadata.Keywords,
			Image:         a.Metadata.Image,
			DateModified:  optionalTime(a.Metadata.DateModified),
			Extra:         a.Metadata.Extra,
		},
	}
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

type ArticleSearchResult struct {
	Article         `json:"article" ` // Embedded Article struct for search results
	Score           float64           `json:"score"`                      // Score rank between 0 and 1
	ScoreNormalized float64           `json:"score_normalized,omitempty"` // ScoreNormalized is the normalized(between 0-1) score
}
