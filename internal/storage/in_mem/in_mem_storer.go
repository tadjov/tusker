package in_mem

import (
	"context"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/types/document"
)

type InMemIndexer struct {
	storageLock sync.RWMutex
	storage     map[uuid.UUID]document.Article
}

func NewInMemIndexer() *InMemIndexer {
	return &InMemIndexer{
		storage: make(map[uuid.UUID]document.Article),
	}
}

func (s *InMemIndexer) Save(_ context.Context, article document.Article) (uuid.UUID, error) {
	// Implement the logic to save the article to a JSON file
	// This is a placeholder implementation
	// You would typically use encoding/json to marshal the article and write it to the file
	slog.Info("Saving article to JSON file", "title", article.Title)
	s.storageLock.Lock()
	defer s.storageLock.Unlock()
	s.storage[article.ID] = article
	// For now, just return a new UUID
	return uuid.New(), nil
}

func (s *InMemIndexer) SaveBulk(ctx context.Context, articles []document.Article) error {
	s.storageLock.Lock()
	defer s.storageLock.Unlock()

	for _, article := range articles {
		if article.ID == uuid.Nil {
			article.ID = uuid.New()
		}
		s.storage[article.ID] = article
		slog.Info("Saving article to in-memory storage", "title", article.Title, "id", article.ID)
	}

	return nil
}
