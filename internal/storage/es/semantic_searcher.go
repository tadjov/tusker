package es

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/tadjov/tusker/internal/api/dto"
	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage"
	dquery "github.com/tadjov/tusker/internal/types/query"
)

const minNumCandidates = 100

// defaultThreshold is the distance ceiling applied when no threshold is set,
// matching the PG semantic searcher.
const defaultThreshold = 0.7

type SemanticSearcher struct {
	client    *elasticsearch.TypedClient
	indexName string
	embedder  *embedding.Embedder
	model     string
}

func NewSemanticSearcher(client *Client, embedder *embedding.Embedder, model string) *SemanticSearcher {
	return &SemanticSearcher{
		client:    client.typed,
		indexName: client.indexName,
		embedder:  embedder,
		model:     model,
	}
}

func (s *SemanticSearcher) SearchSemantic(ctx context.Context, query *dquery.Semantic, baseOpts *dquery.BaseOptions) (*storage.VectorSearchResult, error) {
	vec, err := s.embedder.EmbedQuery(ctx, query.Query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	size := baseOpts.Size
	k := size
	numCandidates := k * 10
	if numCandidates < minNumCandidates {
		numCandidates = minNumCandidates
	}

	threshold := query.Threshold
	if threshold == 0 {
		threshold = defaultThreshold
	}

	// ES kNN filters by minimum cosine similarity (1 - distance), so map the
	// distance threshold to a similarity floor.
	sim := float32(1 - threshold)
	knn := types.KnnSearch{
		Field:         "embedding",
		QueryVector:   vec.Embedding,
		K:             &k,
		NumCandidates: &numCandidates,
		Similarity:    &sim,
		Filter: []types.Query{{
			Term: map[string]types.TermQuery{"embedding_model": {Value: vec.Model}},
		}},
	}

	slog.Info("Executing es semantic kNN search",
		"query", query.Query,
		"model", s.model,
		"k", k,
		"num_candidates", numCandidates,
		"size", size)

	res, err := s.client.Search().
		Index(s.indexName).
		Knn(knn).
		SourceExcludes_("embedding").
		Size(size).
		Do(ctx)
	if err != nil {
		slog.Error("Elasticsearch kNN query failed", "error", err, "query", query.Query)
		return nil, fmt.Errorf("failed to execute semantic search: %w", err)
	}

	hits, err := s.mapToArticles(res.Hits.Hits)
	if err != nil {
		return nil, fmt.Errorf("failed to map semantic search results: %w", err)
	}

	slog.Info("ES semantic search results fetched",
		"total_matches", res.Hits.Total.Value,
		"returned_count", len(hits))

	// kNN returns a single page (parity with PG): no cursor follow-up.
	return &storage.VectorSearchResult{
		Hits:       hits,
		NextCursor: nil,
		HasMore:    false,
	}, nil
}

func (s *SemanticSearcher) mapToArticles(hits []types.Hit) ([]dto.Article, error) {
	articles := make([]dto.Article, 0, len(hits))
	for _, hit := range hits {
		article, err := decodeArticle(hit.Source_)
		if err != nil {
			return nil, err
		}
		articles = append(articles, article)
	}
	return articles, nil
}

var _ storage.SemanticSearcher = (*SemanticSearcher)(nil)
