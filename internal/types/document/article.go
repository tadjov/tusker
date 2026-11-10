package document

import (
	"time"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/types/language"
)

// Article holds only fields an engine filters, sorts or ranks on; the rest go in Metadata.
type Article struct {
	ID              uuid.UUID     `json:"id"`
	URL             string        `json:"url"`
	Title           string        `json:"title"`
	Description     string        `json:"description,omitempty"`
	Content         string        `json:"content"`
	PublishedAt     time.Time     `json:"publishedAt,omitzero"`
	Language        language.Code `json:"language"`
	PublisherDomain string        `json:"publisherDomain,omitempty"`
	Metadata        Metadata      `json:"metadata,omitzero"`
}

// Metadata is stored and returned, never searched.
type Metadata struct {
	// ExternalID is the source dataset's own id.
	ExternalID    string         `json:"externalId,omitempty"`
	PublisherName string         `json:"publisherName,omitempty"`
	Authors       []string       `json:"authors,omitempty"`
	Section       string         `json:"section,omitempty"`
	Keywords      []string       `json:"keywords,omitempty"`
	Image         string         `json:"image,omitempty"`
	DateModified  time.Time      `json:"dateModified,omitzero"`
	Extra         map[string]any `json:"extra,omitempty"`
}

// ArticleIDNamespace seeds every derived article id. Changing it changes the id
// of every article in every corpus built with a derived strategy.
var ArticleIDNamespace = uuid.MustParse("697bf49f-ca14-4303-83eb-f9d05f7c7c42")

// DeriveArticleID returns the UUIDv5 of name, so the same name always yields the
// same id and preprocess can be re-run without renumbering a corpus.
func DeriveArticleID(name string) uuid.UUID {
	return uuid.NewSHA1(ArticleIDNamespace, []byte(name))
}
