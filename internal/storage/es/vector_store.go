package es

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/embedding"
)

// VectorStore is the Elasticsearch implementation of storage.VectorStore.
// Unlike Postgres (a separate article_embeddings table), the document vector
// lives as a dense_vector field on the article document itself, keyed by
// _id == article UUID and written by Embedder. Queries are embedded at runtime
// with the same model the stored vectors were produced with, so query and
// document vectors stay comparable.
type VectorStore struct {
	client    *elasticsearch.TypedClient
	indexName string
	embedder  *embedding.Embedder
	model     string
}

func NewVectorStore(config ClientConfig, embedder *embedding.Embedder, model string) (*VectorStore, error) {
	client, err := newTypedClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Elasticsearch client: %w", err)
	}
	return &VectorStore{
		client:    client,
		indexName: config.IndexName,
		embedder:  embedder,
		model:     model,
	}, nil
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

	idStrs := make([]string, len(ids))
	for i, id := range ids {
		idStrs[i] = id.String()
	}

	res, err := s.client.Search().
		Index(s.indexName).
		Query(&types.Query{Ids: &types.IdsQuery{Values: idStrs}}).
		SourceIncludes_("id", "embedding", "embedding_model").
		Size(len(ids)).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("query article embeddings: %w", err)
	}

	for _, hit := range res.Hits.Hits {
		var src struct {
			ID        string    `json:"id"`
			Embedding []float32 `json:"embedding"`
			Model     string    `json:"embedding_model"`
		}
		if err := json.Unmarshal(hit.Source_, &src); err != nil {
			return nil, fmt.Errorf("unmarshal embedding source: %w", err)
		}
		// Query and document vectors must come from the same model; skip any
		// document embedded with a different one (mirrors the PG model filter).
		if s.model != "" && src.Model != "" && src.Model != s.model {
			continue
		}
		if len(src.Embedding) == 0 {
			continue
		}
		id, err := uuid.Parse(src.ID)
		if err != nil {
			return nil, fmt.Errorf("parse embedding doc id %q: %w", src.ID, err)
		}
		out[id] = src.Embedding
	}

	return out, nil
}

func (s *VectorStore) Model() string {
	return s.model
}
