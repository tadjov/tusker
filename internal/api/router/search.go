package router

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/tadjov/tusker/internal/api/dto"
	"github.com/tadjov/tusker/internal/apperr"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/types/operator"
	dquery "github.com/tadjov/tusker/internal/types/query"
)

type SearchRouter struct {
	e                *echo.Echo
	searcher         storage.FtsSearcher
	semanticSearcher storage.SemanticSearcher
	hybridSearcher   storage.HybridSearcher
}

type SearchRouterOption func(*SearchRouter)

func NewSearchRouter(e *echo.Echo, searcher storage.FtsSearcher, opts ...SearchRouterOption) *SearchRouter {
	router := &SearchRouter{
		e:        e,
		searcher: searcher,
	}

	for _, opt := range opts {
		opt(router)
	}

	return router
}

func WithSemanticSearcher(searcher storage.SemanticSearcher) SearchRouterOption {
	return func(r *SearchRouter) {
		r.semanticSearcher = searcher
	}
}

func WithHybridSearcher(searcher storage.HybridSearcher) SearchRouterOption {
	return func(r *SearchRouter) {
		r.hybridSearcher = searcher
	}
}

func (r *SearchRouter) Bind() {
	// Simple query_string API (application-determined fields/weights)
	r.e.GET("/v1/articles/search", r.searchHandler)

	// Unified structured search API (match/multi_match with query wrapper)
	r.e.POST("/v1/articles/_search", r.structuredSearchHandler)

	// Semantic search endpoint (only if provided via options)
	if r.semanticSearcher != nil {
		r.e.GET("/v1/articles/semantic_search", r.handleSematicQuery)
	}

	// Capabilities discovery endpoint
	r.e.GET("/v1/capabilities", r.capabilitiesHandler)
}

// capabilitiesHandler reports what the running backend supports (GET)
//
// The full-text searcher is always wired, so query_string, match, multi_match,
// phrase and boolean are always available; semantic and hybrid only when their
// searchers are. full_text is what the engine itself declares it can honour.
//
// @Summary Report supported query types and parameters
// @Description Returns the query types the running backend serves and the full-text parameters its engine honours: the operator it applies when a request names none, whether it supports fuzziness, phrase slop and boosts, and the fields it can match apart from every other field. A stated parameter outside these is a 400 unless the request sends `Prefer: handling=lenient`.
// @Tags capabilities
// @Produce json
// @Success 200 {object} dto.CapabilitiesResponse "Supported query types and parameters"
// @Router /v1/capabilities [get]
func (r *SearchRouter) capabilitiesHandler(c echo.Context) error {
	return c.JSON(http.StatusOK, dto.CapabilitiesResponse{
		QueryTypes: dto.QueryTypes{
			StringQuery: true,
			Match:       true,
			MultiMatch:  true,
			Phrase:      true,
			Boolean:     true,
			Semantic:    r.semanticSearcher != nil,
			Hybrid:      r.hybridSearcher != nil,
		},
		FullText: r.searcher.Capabilities(),
	})
}

// paramQuery is a query whose stated params an engine may not honour.
type paramQuery interface {
	Params() dquery.Params
	Apply(dquery.Params)
}

// honour applies the request's Prefer handling to the params of q the engine
// cannot honour: an error for a strict request; for a lenient one q loses them
// before it runs and they are returned as ignored. A nil q states no params.
func (r *SearchRouter) honour(c echo.Context, q paramQuery) ([]dquery.Ignored, error) {
	handling := dto.HandlingFromPrefer(c.Request().Header.Values("Prefer"))
	if handling == dquery.HandlingLenient {
		c.Response().Header().Set("Preference-Applied", "handling=lenient")
	}
	var stated dquery.Params
	if q != nil {
		stated = q.Params()
	}
	honoured, ignored, err := dquery.CheckCapabilities(r.searcher.Capabilities(), stated, handling)
	if err != nil {
		return nil, apperr.NewValidationWrap(err.Error(), err)
	}
	if q != nil {
		q.Apply(honoured)
	}
	return ignored, nil
}

// searchHandler handles simple query string search (GET)
//
// This endpoint provides a simple, Google-like search experience.
// The application automatically determines optimal fields and weights.
// Results are cacheable and URLs are bookmarkable.
//
// @Summary Simple query string search
// @Description Free-text search over title, description and content: operators and quotes are read as words. Terms combine under the engine's default operator (PostgreSQL AND, Elasticsearch OR); the response echoes it in `effective`. Any query parameter not listed here is a 400. Cacheable and bookmarkable.
// @Tags search
// @Accept json
// @Produce json
// @Param q query string true "SearchStringQuery query text" example("climate change")
// @Param size query int false "Results per page (default: 100, max: 10000)" example(10)
// @Param cursor query string false "Pagination cursor (base64-encoded from previous response)"
// @Param lang query string false "ISO 639-3 language code; only eng today (default: eng)" example("eng")
// @Param Prefer header string false "handling=lenient drops a parameter the engine cannot honour and lists it in ignored, instead of a 400"
// @Success 200 {object} dto.SearchResponse "SearchStringQuery results with pagination metadata"
// @Failure 400 {object} map[string]string "Bad request - missing or invalid parameters"
// @Failure 500 {object} map[string]string "Internal server error"
// @Router /v1/articles/search [get]
// @Example Request:  GET /v1/articles/search?q=climate%20change&size=10&lang=eng
// @Example Response: {"hits": [...], "next_cursor": "eyJ...", "has_more": true, "total_matches": 1523}
func (r *SearchRouter) searchHandler(c echo.Context) error {
	if err := dto.CheckSearchBoxParams(c.QueryParams()); err != nil {
		return err
	}

	// Support both 'q' (preferred) and 'query' (legacy) parameters
	params := dto.StringQueryParams{
		Query:    c.QueryParam("q"),
		Language: c.QueryParam("lang"),
	}
	if params.Query == "" {
		params.Query = c.QueryParam("query") // Backward compatibility
	}

	queryString, err := params.ToDomain()
	if err != nil {
		return err
	}

	sizeInt, err := dto.ParsePageSize(c.QueryParam("size"))
	if err != nil {
		return err
	}

	var cursor *dquery.Cursor
	if cursorStr := c.QueryParam("cursor"); cursorStr != "" {
		cursor, err = dquery.DecodeCursor(cursorStr)
		if err != nil {
			return apperr.NewValidation("invalid cursor parameter")
		}
	}

	ignored, err := r.honour(c, nil)
	if err != nil {
		return err
	}

	searchResult, err := r.searcher.SearchStringQuery(c.Request().Context(), queryString, &dquery.BaseOptions{
		Cursor: cursor,
		Size:   sizeInt,
	})
	if err != nil {
		slog.Error("Failed to execute full-text search", "error", err, "query", params.Query)
		return err
	}

	return r.buildResponse(c, searchResult, contractReport{
		effective: queryString.Effective(r.defaultOperator()),
		ignored:   ignored,
	})
}

// structuredSearchHandler handles structured search requests (POST)
//
// This endpoint accepts complex, structured queries with explicit control over:
// - Which fields to search
// - Field-level weights/boosting
// - Operator logic (AND/OR)
// - Language-specific analysis
// - Fuzziness/typo tolerance
//
// Supports multiple query types via the query wrapper pattern.
// Query types: match, multi_match (more coming: bool, phrase, query_string)
//
// @Summary Structured search API
// @Description Structured match, multi_match, phrase, boolean or hybrid search. A stated operator, field, fuzziness or language is honoured by the engine or the request is a 400 naming the parameter and the engine. An omitted operator takes the engine's default (PostgreSQL AND, Elasticsearch OR). Boosts are best effort. A key the query type does not define is a 400. With `Prefer: handling=lenient` an unsupported parameter is removed before the search runs and listed in `ignored`. Every response echoes `effective` and `ignored`.
// @Tags search
// @Accept json
// @Produce json
// @Param request body dto.SearchRequest true "Structured search request with query wrapper"
// @Param Prefer header string false "handling=lenient drops a parameter the engine cannot honour and lists it in ignored, instead of a 400"
// @Success 200 {object} dto.SearchResponse "SearchStringQuery results with pagination metadata"
// @Failure 400 {object} map[string]string "Bad request - invalid query structure or parameters"
// @Failure 500 {object} map[string]string "Internal server error"
// @Failure 501 {object} map[string]string "Query type not supported by searcher backend"
// @Router /v1/articles/_search [post]
// @Example Match Request:
//
//	{
//	  "size": 10,
//	  "query": {
//	    "match": {
//	      "field": "title",
//	      "query": "climate change",
//	      "operator": "and",
//	      "fuzziness": "AUTO",
//	      "language": "eng"
//	    }
//	  }
//	}
//
// @Example MultiMatch Request:
//
//	{
//	  "size": 10,
//	  "query": {
//	    "multi_match": {
//	      "query": "renewable energy",
//	      "fields": ["title^3", "description^2", "content"],
//	      "operator": "or",
//	      "language": "eng"
//	    }
//	  }
//	}
func (r *SearchRouter) structuredSearchHandler(c echo.Context) error {
	if ctype := c.Request().Header.Get(echo.HeaderContentType); !strings.HasPrefix(ctype, echo.MIMEApplicationJSON) {
		return echo.ErrUnsupportedMediaType
	}
	req, err := dto.DecodeSearchRequest(c.Request().Body)
	if err != nil {
		// The body limit surfaces as a read error and keeps its own status.
		var httpErr *echo.HTTPError
		if errors.As(err, &httpErr) {
			return httpErr
		}
		return err
	}

	if err := c.Validate(req); err != nil {
		return err
	}

	sizeInt, err := dto.PageSize(req.Size)
	if err != nil {
		return err
	}

	var cursor *dquery.Cursor
	if req.Cursor != "" {
		cursor, err = dquery.DecodeCursor(req.Cursor)
		if err != nil {
			return apperr.NewValidation("invalid cursor parameter")
		}
	}

	opts := &dquery.BaseOptions{
		Cursor: cursor,
		Size:   sizeInt,
	}

	queryType := req.Query.GetQueryType()
	switch queryType {
	case dquery.MatchType:
		return r.handleMatchQuery(c, req.Query.Match, opts)
	case dquery.MultiMatchType:
		return r.handleMultiMatchQuery(c, req.Query.MultiMatch, opts)
	case dquery.PhraseType:
		return r.handlePhraseQuery(c, req.Query.Phrase, opts)
	case dquery.BooleanType:
		return r.handleBooleanQuery(c, req.Query.Boolean, opts)
	case dquery.HybridType:
		return r.handleHybridQuery(c, req.Query.Hybrid, opts)
	default:
		return apperr.NewValidation("query must specify one of: match, multi_match, phrase, boolean, hybrid")
	}
}

func (r *SearchRouter) handleHybridQuery(c echo.Context, params *dto.HybridParams, options *dquery.BaseOptions) error {
	if r.hybridSearcher == nil {
		return apperr.NewValidation("hybrid search is not enabled on this server")
	}

	domainQuery, err := params.ToDomain()
	if err != nil {
		return err
	}

	ignored, err := r.honour(c, nil)
	if err != nil {
		return err
	}

	searchResult, err := r.hybridSearcher.SearchHybrid(c.Request().Context(), domainQuery, options)
	if err != nil {
		slog.Error("Failed to execute hybrid search", "error", err, "query", params.Query)
		return err
	}

	return r.buildResponse(c, searchResult, contractReport{
		effective: domainQuery.Effective(r.defaultOperator()),
		ignored:   ignored,
	})
}

func (r *SearchRouter) handleMatchQuery(c echo.Context, params *dto.MatchParams, options *dquery.BaseOptions) error {
	domainQuery, err := params.ToDomain()
	if err != nil {
		return err
	}

	ignored, err := r.honour(c, domainQuery)
	if err != nil {
		return err
	}

	searchResult, err := r.searcher.SearchField(c.Request().Context(), domainQuery, options)
	if err != nil {
		slog.Error("Failed to execute match search", "error", err, "field", params.Field, "query", params.Query)
		return err
	}

	return r.buildResponse(c, searchResult, contractReport{
		effective: domainQuery.Effective(r.defaultOperator()),
		ignored:   ignored,
	})
}

func (r *SearchRouter) handleMultiMatchQuery(c echo.Context, params *dto.MultiMatchParams, options *dquery.BaseOptions) error {
	domainQuery, err := params.ToDomain()
	if err != nil {
		return err
	}

	ignored, err := r.honour(c, domainQuery)
	if err != nil {
		return err
	}

	searchResult, err := r.searcher.SearchFields(c.Request().Context(), domainQuery, options)
	if err != nil {
		slog.Error("Failed to execute multi_match search", "error", err, "fields", params.Fields, "query", params.Query)
		return err
	}

	return r.buildResponse(c, searchResult, contractReport{
		effective: domainQuery.Effective(r.defaultOperator()),
		ignored:   ignored,
	})
}

func (r *SearchRouter) handlePhraseQuery(c echo.Context, params *dto.PhraseParams, options *dquery.BaseOptions) error {
	domainQuery, err := params.ToDomain()
	if err != nil {
		return err
	}

	ignored, err := r.honour(c, domainQuery)
	if err != nil {
		return err
	}

	searchResult, err := r.searcher.SearchPhrase(c.Request().Context(), domainQuery, options)
	if err != nil {
		slog.Error("Failed to execute phrase search", "error", err, "fields", params.Fields, "query", params.Query, "slop", params.Slop)
		return err
	}

	return r.buildResponse(c, searchResult, contractReport{
		effective: domainQuery.Effective(),
		ignored:   ignored,
	})
}

func (r *SearchRouter) handleBooleanQuery(c echo.Context, params *dto.BooleanParams, options *dquery.BaseOptions) error {
	domainQuery, err := params.ToDomain()
	if err != nil {
		return err
	}

	ignored, err := r.honour(c, nil)
	if err != nil {
		return err
	}

	searchResult, err := r.searcher.SearchBoolean(c.Request().Context(), domainQuery, options)
	if err != nil {
		slog.Error("Failed to execute boolean search", "error", err, "expression", params.Expression)
		return err
	}

	return r.buildResponse(c, searchResult, contractReport{
		effective: domainQuery.Effective(r.defaultOperator()),
		ignored:   ignored,
	})
}

// handleSematicQuery handles semantic query search (GET)
//
// This endpoint provides a vector-based semantic search experience using the embedding model.
// The application automatically determines optimal search strategy based on index configuration.
// Results are cacheable and URLs are bookmarkable.
//
// @Summary Vector-based semantic search
// @Description Perform vector-based semantic search using the embedding model. Supports pagination via cursor. Application determines optimal search strategy based on index configuration. Results are cacheable and bookmarkable.
// @Tags search
// @Accept json
// @Produce json
// @Param q query string true "SearchStringQuery query text" example("climate change")
// @Param size query int false "Results per page (default: 100, max: 10000)" example(10)
// @Param cursor query string false "Pagination cursor (base64-encoded from previous response)"
// @Success 200 {object} dto.SemanticSearchResponse "SearchStringQuery results with pagination metadata"
// @Failure 400 {object} map[string]string "Bad request - missing or invalid parameters"
// @Failure 500 {object} map[string]string "Internal server error"
// @Router /v1/articles/semantic_search [get]
// @Example Request:  GET /v1/articles/semantic_search?q=climate%20change&size=10
// @Example Response: {"hits": [...], "next_cursor": "eyJ...", "has_more": true}
func (r *SearchRouter) handleSematicQuery(c echo.Context) error {
	query := c.QueryParam("q")
	if query == "" {
		return apperr.NewValidation("q parameter is required")
	}

	sizeStr := c.QueryParam("size")
	size, err := dto.ParsePageSize(sizeStr)
	if err != nil {
		return err
	}

	cursorStr := c.QueryParam("cursor")

	req := dto.SemanticSearchRequest{
		Query:  query,
		Size:   size,
		Cursor: cursorStr,
	}

	var cursor *dquery.Cursor
	if req.Cursor != "" {
		var err error
		cursor, err = dquery.DecodeCursor(req.Cursor)
		if err != nil {
			return apperr.NewValidation("invalid cursor parameter")
		}
	}

	options := &dquery.BaseOptions{
		Cursor: cursor,
		Size:   size,
	}

	domainQuery, err := req.ToDomain()
	if err != nil {
		return err
	}

	searchResult, err := r.semanticSearcher.SearchSemantic(c.Request().Context(), domainQuery, options)
	if err != nil {
		slog.Error("Failed to execute semantic search", "error", err, "query", req.Query)
		return err
	}

	var nextCursorStr *string
	if searchResult.NextCursor != nil {
		encoded, err := dquery.EncodeCursor(searchResult.NextCursor.Score, searchResult.NextCursor.ID)
		if err != nil {
			slog.Error("Failed to encode cursor", "error", err)
			return fmt.Errorf("failed to encode cursor: %w", err)
		}
		nextCursorStr = &encoded
	}

	hits := searchResult.Hits
	if hits == nil {
		hits = []dto.Article{}
	}

	apiResponse := dto.SemanticSearchResponse{
		Hits:       hits,
		NextCursor: nextCursorStr,
		HasMore:    searchResult.HasMore,
	}

	return c.JSON(http.StatusOK, apiResponse)
}

func (r *SearchRouter) defaultOperator() operator.Operator {
	return r.searcher.Capabilities().DefaultOperator
}

// contractReport is what a search response says about the contract it ran under.
type contractReport struct {
	effective dquery.Effective
	ignored   []dquery.Ignored
}

func (r *SearchRouter) buildResponse(c echo.Context, searchResult *storage.SearchResult, report contractReport) error {
	var nextCursorStr *string
	if searchResult.NextCursor != nil {
		encoded, err := dquery.EncodeCursor(searchResult.NextCursor.Score, searchResult.NextCursor.ID)
		if err != nil {
			slog.Error("Failed to encode cursor", "error", err)
			return fmt.Errorf("failed to encode cursor: %w", err)
		}
		nextCursorStr = &encoded
	}

	hits := searchResult.Hits
	if hits == nil {
		hits = []dto.ArticleSearchResult{}
	}

	ignored := report.ignored
	if ignored == nil {
		ignored = []dquery.Ignored{}
	}

	apiResponse := dto.SearchResponse{
		Hits:         hits,
		NextCursor:   nextCursorStr,
		HasMore:      searchResult.HasMore,
		MaxScore:     searchResult.MaxScore,
		PageMaxScore: searchResult.PageMaxScore,
		TotalMatches: searchResult.TotalMatches,
		Effective:    report.effective,
		Ignored:      ignored,
	}

	return c.JSON(http.StatusOK, apiResponse)
}
