package storage

import (
	"context"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/types/document"
)

type Indexer interface {
	Save(ctx context.Context, article document.Article) (uuid.UUID, error)
	SaveBulk(ctx context.Context, articles []document.Article) error
}

type Type string

const (
	ES    Type = "es"
	PG    Type = "pg"
	Solr  Type = "solr"
	InMem Type = "in_mem"
)

type StorerError string

const (
	ErrUnsupportedStorer StorerError = "unsupported storer type: %s"
)

func (e StorerError) Error() string {
	return string(e)
}

// EmbeddedDocumentCounter is an optional capability for backends that keep vectors on
// the article document, where an article write overwrites them. Postgres does not.
type EmbeddedDocumentCounter interface {
	CountEmbedded(ctx context.Context) (int64, error)
}

type EmbedIndexer interface {
	Save(ctx context.Context, article *embedding.Vec) (uuid.UUID, error)
	SaveBulk(ctx context.Context, article []*embedding.Vec) (EmbedWriteResult, error)
}

// EmbedWriteResult reports what a bulk write stored. Both backends skip vectors
// whose article is absent, so a run can succeed and store nothing.
type EmbedWriteResult struct {
	Stored  int
	Skipped int
}
