package storage

import (
	"context"

	"github.com/tadjov/tusker/internal/api/dto"
	"github.com/tadjov/tusker/internal/types/query"
)

// SearchResult represents search results with cursor-based pagination
// Contains types objects - no encoding/decoding at this layer
type SearchResult struct {
	Hits         []dto.ArticleSearchResult `json:"hits"`
	NextCursor   *query.Cursor             `json:"-"`
	HasMore      bool                      `json:"has_more"`
	MaxScore     float64                   `json:"max_score"`
	PageMaxScore float64                   `json:"page_max_score,omitempty"`
	TotalMatches int64                     `json:"total_matches,omitempty"`
}

type VectorSearchResult struct {
	Hits       []dto.Article `json:"hits"`
	NextCursor *query.Cursor `json:"-"`
	HasMore    bool          `json:"has_more"`
}

// FtsSearcher is the full text API interface
// Provides full-text search capability
type FtsSearcher interface {
	// SearchStringQuery performs simple string-based search with application-optimized settings
	// The storage implementation determines optimal fields, weights, and search strategy
	SearchStringQuery(ctx context.Context, query *query.String, baseOpts *query.BaseOptions) (*SearchResult, error)
	// SearchField performs single-field match query with relevance scoring
	SearchField(ctx context.Context, query *query.Match, baseOpts *query.BaseOptions) (*SearchResult, error)
	// SearchFields performs multi-field match query with per-field weighting
	SearchFields(ctx context.Context, query *query.MultiMatch, baseOpts *query.BaseOptions) (*SearchResult, error)
	// SearchPhrase performs phrase search on specified fields with slop support
	// Elasticsearch: Uses match_phrase query with slop parameter
	// PostgreSQL: Uses phraseto_tsquery (slop=0) or to_tsquery with distance operators (slop>0)
	SearchPhrase(ctx context.Context, query *query.Phrase, baseOpts *query.BaseOptions) (*SearchResult, error)
	// SearchBoolean performs complex boolean queries combining multiple conditions
	SearchBoolean(ctx context.Context, query *query.Boolean, baseOpts *query.BaseOptions) (*SearchResult, error)
	// Capabilities reports which stated parameters the searcher honours.
	Capabilities() query.Capabilities
}

type EmbedResult struct {
	Hits       []dto.ArticleSearchResult `json:"hits"`
	NextCursor *query.Cursor             `json:"-"`
	HasMore    bool                      `json:"has_more"`
}

// SemanticSearcher is the semantic search API interface
type SemanticSearcher interface {
	// SearchSemantic performs semantic search using vector embeddings
	SearchSemantic(ctx context.Context, query *query.Semantic, baseOpts *query.BaseOptions) (*VectorSearchResult, error)
}

// HybridSearcher combines lexical FTS with vector similarity via RRF.
type HybridSearcher interface {
	SearchHybrid(ctx context.Context, query *query.Hybrid, baseOpts *query.BaseOptions) (*SearchResult, error)
}
