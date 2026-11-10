package es

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/operator"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/sortorder"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/textquerytype"
	"github.com/tadjov/tusker/internal/api/dto"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/token"
	queryoperator "github.com/tadjov/tusker/internal/types/operator"
	dquery "github.com/tadjov/tusker/internal/types/query"
	"github.com/tadjov/tusker/pkg/utils"
)

type Searcher struct {
	client    *elasticsearch.TypedClient
	indexName string
	tokenizer *token.BoolTokenizer
}

func NewSearcher(client *Client) *Searcher {
	return &Searcher{
		client:    client.typed,
		indexName: client.indexName,
		tokenizer: token.NewBoolTokenizer(),
	}
}

// DefaultOperator is what Elasticsearch's match and multi_match apply when a
// request names no operator.
const DefaultOperator = queryoperator.Or

// Capabilities reports every searchable field as isolated: each is its own
// field in the index mapping.
func (r *Searcher) Capabilities() dquery.Capabilities {
	return dquery.Capabilities{
		Engine:          "elasticsearch",
		DefaultOperator: DefaultOperator,
		Fuzziness:       true,
		PhraseSlop:      true,
		Boosts:          true,
		Fields:          dquery.SearchableFields(),
	}
}

// SearchStringQuery matches the query's SearchContract with a cross_fields
// multi_match, so each term may hit any contract field, as it does in the
// Postgres search_vector. The contract language is not applied: analysis is
// fixed per field by the index mapping.
func (r *Searcher) SearchStringQuery(ctx context.Context, query *dquery.String, baseOpts *dquery.BaseOptions) (*storage.SearchResult, error) {
	cursor, size := baseOpts.Cursor, baseOpts.Size
	contract := query.Contract()

	slog.Info("Executing es query_string search",
		"query", query.Query,
		"fields", contract.Fields,
		"operator", queryoperator.Resolve(contract.Operator, DefaultOperator),
		"language", contract.Language,
		"has_cursor", cursor != nil,
		"size", size)

	multiMatch := &types.MultiMatchQuery{
		Query:    query.Query,
		Fields:   boostedFields(contract.Fields),
		Type:     &textquerytype.Crossfields,
		Operator: termOperator(contract.Operator),
	}

	searchReq := r.client.Search().
		Index(r.indexName).
		SourceExcludes_("embedding").
		Query(&types.Query{
			MultiMatch: multiMatch,
		}).
		Size(size + 1).
		TrackScores(true).
		TrackTotalHits(true)

	if cursor != nil {
		searchReq = searchReq.SearchAfter(
			types.FieldValue(cursor.Score),
			types.FieldValue(cursor.ID.String()),
		)
	}

	sortOrderDesc := sortorder.Desc
	searchReq = searchReq.Sort(
		&types.SortOptions{
			SortOptions: map[string]types.FieldSort{
				"_score": {Order: &sortOrderDesc},
			},
		},
		&types.SortOptions{
			SortOptions: map[string]types.FieldSort{
				"id": {Order: &sortOrderDesc},
			},
		},
	)

	var err error

	res, err := searchReq.Do(ctx)
	if err != nil {
		slog.Error("Elasticsearch query failed", "error", err, "query", query.Query, "cursor", cursor != nil)
		return nil, fmt.Errorf("failed to execute search: %w", err)
	}

	page, err := searchResultPage(res.Hits, size)
	if err != nil {
		return nil, fmt.Errorf("failed to map search results to types: %w", err)
	}

	slog.Info("Es search results fetched",
		"total_matches", page.TotalMatches,
		"returned_count", len(page.Hits),
		"max_score", page.MaxScore)

	return page, nil
}

func boostedFields(fields []dquery.FieldWeight) []string {
	boosted := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.Weight == 1.0 {
			boosted = append(boosted, string(f.Field))
			continue
		}
		boosted = append(boosted, fmt.Sprintf("%s^%g", f.Field, f.Weight))
	}
	return boosted
}

// termOperator is nil when the request names no operator, so Elasticsearch
// applies its own default.
func termOperator(op *queryoperator.Operator) *operator.Operator {
	if op == nil {
		return nil
	}
	termOp := operator.And
	if op.IsOr() {
		termOp = operator.Or
	}
	return &termOp
}

// searchResultPage expects hits fetched with size+1: the extra hit only signals
// that another page exists.
func searchResultPage(hits types.HitsMetadata, size int) (*storage.SearchResult, error) {
	articles, rawScores, err := mapToResult(hits.Hits, dquery.CalcSafeScore((*float64)(hits.MaxScore)))
	if err != nil {
		return nil, err
	}

	hasMore := len(articles) > size
	if hasMore {
		articles = articles[:size]
		rawScores = rawScores[:size]
	}

	page := &storage.SearchResult{
		Hits:    articles,
		HasMore: hasMore,
	}
	if hits.Total != nil {
		page.TotalMatches = hits.Total.Value
	}
	if hits.MaxScore != nil {
		page.MaxScore = utils.RoundFloat64(float64(*hits.MaxScore), dquery.ScoreDecimalPlaces)
	}
	if len(articles) > 0 {
		page.PageMaxScore = utils.RoundFloat64(rawScores[0], dquery.ScoreDecimalPlaces)
	}
	if hasMore && len(articles) > 0 {
		page.NextCursor = &dquery.Cursor{
			Score: rawScores[len(rawScores)-1],
			ID:    articles[len(articles)-1].Article.ID,
		}
	}
	return page, nil
}

func mapToResult(hits []types.Hit, maxScore float64) ([]dto.ArticleSearchResult, []float64, error) {
	articles := make([]dto.ArticleSearchResult, 0, len(hits))
	rawScores := make([]float64, 0, len(hits))

	for _, hit := range hits {
		article, err := decodeArticle(hit.Source_)
		if err != nil {
			return nil, nil, err
		}
		if hit.Score_ == nil {
			return nil, nil, fmt.Errorf("document %s has no _score", article.ID)
		}

		rawScore := float64(*hit.Score_)
		articles = append(articles, dto.ArticleSearchResult{
			Article:         article,
			ScoreNormalized: rawScore / maxScore,
			Score:           rawScore,
		})
		rawScores = append(rawScores, rawScore)
	}

	return articles, rawScores, nil
}

// SearchField implements storage.SingleMatchSearcher interface
// Performs single-field match query using Elasticsearch's match query
func (r *Searcher) SearchField(ctx context.Context, query *dquery.Match, baseOpts *dquery.BaseOptions) (*storage.SearchResult, error) {
	cursor, size := baseOpts.Cursor, baseOpts.Size
	slog.Info("Executing es match search",
		"query", query.Query,
		"field", query.Field,
		"operator", queryoperator.Resolve(query.Operator, DefaultOperator),
		"fuzziness", query.GetFuzziness(),
		"has_cursor", cursor != nil,
		"size", size)

	matchQuery := &types.MatchQuery{
		Query:    query.Query,
		Operator: termOperator(query.Operator),
	}

	if fuzziness := query.GetFuzziness(); fuzziness != dquery.NoFuzziness {
		editDistance := string(fuzziness)
		matchQuery.Fuzziness = &editDistance
	}

	// Build search request with match query on specific field
	searchReq := r.client.Search().
		Index(r.indexName).
		SourceExcludes_("embedding").
		Query(&types.Query{
			Match: map[string]types.MatchQuery{
				string(query.Field): *matchQuery,
			},
		}).
		Size(size + 1).
		TrackScores(true).
		TrackTotalHits(true)

	// Add cursor support and sorting
	if cursor != nil {
		searchReq = searchReq.SearchAfter(
			types.FieldValue(cursor.Score),
			types.FieldValue(cursor.ID.String()),
		)
	}

	sortOrderDesc := sortorder.Desc
	searchReq = searchReq.Sort(
		&types.SortOptions{
			SortOptions: map[string]types.FieldSort{
				"_score": {Order: &sortOrderDesc},
			},
		},
		&types.SortOptions{
			SortOptions: map[string]types.FieldSort{
				"id": {Order: &sortOrderDesc},
			},
		},
	)

	// Execute query
	res, err := searchReq.Do(ctx)
	if err != nil {
		slog.Error("Elasticsearch match query failed", "error", err, "query", query.Query, "field", query.Field)
		return nil, fmt.Errorf("failed to execute match search: %w", err)
	}

	page, err := searchResultPage(res.Hits, size)
	if err != nil {
		return nil, fmt.Errorf("failed to map search results to types: %w", err)
	}

	slog.Info("ES match search results fetched",
		"total_matches", page.TotalMatches,
		"returned_count", len(page.Hits),
		"max_score", page.MaxScore)

	return page, nil
}

// SearchFields implements storage.MultiMatchSearcher interface
// Performs multi-field match query using Elasticsearch's multi_match query
func (r *Searcher) SearchFields(ctx context.Context, query *dquery.MultiMatch, baseOpts *dquery.BaseOptions) (*storage.SearchResult, error) {
	cursor, size := baseOpts.Cursor, baseOpts.Size
	slog.Info("Executing es multi_match search",
		"query", query.Query,
		"fields", query.Fields,
		"operator", queryoperator.Resolve(query.Operator, DefaultOperator),
		"has_cursor", cursor != nil,
		"size", size)

	// cross_fields lets each term match any of the fields, as the Postgres
	// search_vector does; the default best_fields wants every term in one.
	multiMatch := &types.MultiMatchQuery{
		Query:    query.Query,
		Fields:   boostedFields(query.Params().Fields),
		Type:     &textquerytype.Crossfields,
		Operator: termOperator(query.Operator),
	}

	// Build and execute search request
	searchReq := r.client.Search().
		Index(r.indexName).
		SourceExcludes_("embedding").
		Query(&types.Query{
			MultiMatch: multiMatch,
		}).
		Size(size + 1).
		TrackScores(true).
		TrackTotalHits(true)

	// Add cursor support and sorting
	if cursor != nil {
		searchReq = searchReq.SearchAfter(
			types.FieldValue(cursor.Score),
			types.FieldValue(cursor.ID.String()),
		)
	}

	sortOrderDesc := sortorder.Desc
	searchReq = searchReq.Sort(
		&types.SortOptions{
			SortOptions: map[string]types.FieldSort{
				"_score": {Order: &sortOrderDesc},
			},
		},
		&types.SortOptions{
			SortOptions: map[string]types.FieldSort{
				"id": {Order: &sortOrderDesc},
			},
		},
	)

	// Execute query
	res, err := searchReq.Do(ctx)
	if err != nil {
		slog.Error("Elasticsearch multi_match query failed", "error", err, "query", query.Query)
		return nil, fmt.Errorf("failed to execute multi_match search: %w", err)
	}

	page, err := searchResultPage(res.Hits, size)
	if err != nil {
		return nil, fmt.Errorf("failed to map search results to types: %w", err)
	}

	slog.Info("ES multi_match search results fetched",
		"total_matches", page.TotalMatches,
		"returned_count", len(page.Hits),
		"max_score", page.MaxScore)

	return page, nil
}

// SearchPhrase implements storage.FtsSearcher interface
// Performs phrase search using Elasticsearch's match_phrase query with slop support
func (r *Searcher) SearchPhrase(ctx context.Context, query *dquery.Phrase, baseOpts *dquery.BaseOptions) (*storage.SearchResult, error) {
	cursor, size := baseOpts.Cursor, baseOpts.Size
	slop := query.GetSlop()

	slog.Info("Executing es phrase search",
		"query", query.Query,
		"fields", query.Fields,
		"slop", slop,
		"language", query.GetLanguage(),
		"has_cursor", cursor != nil,
		"size", size)

	// Build bool query with should clauses for each field
	// Each field gets a match_phrase query with the same slop
	shouldClauses := make([]types.Query, 0, len(query.Fields))
	for _, field := range query.Fields {
		matchPhraseQuery := types.MatchPhraseQuery{
			Query: query.Query,
		}
		if slop > 0 {
			matchPhraseQuery.Slop = &slop
		}

		shouldClauses = append(shouldClauses, types.Query{
			MatchPhrase: map[string]types.MatchPhraseQuery{
				string(field): matchPhraseQuery,
			},
		})
	}

	// Build bool query - at least one field should match
	boolQuery := &types.BoolQuery{
		Should:             shouldClauses,
		MinimumShouldMatch: "1",
	}

	slog.Debug("Elasticsearch phrase query",
		"fields", query.Fields,
		"slop", slop,
		"num_should_clauses", len(shouldClauses))

	// Build search request
	searchReq := r.client.Search().
		Index(r.indexName).
		SourceExcludes_("embedding").
		Query(&types.Query{
			Bool: boolQuery,
		}).
		Size(size + 1).
		TrackScores(true).
		TrackTotalHits(true)

	// Add cursor support
	if cursor != nil {
		searchReq = searchReq.SearchAfter(
			types.FieldValue(cursor.Score),
			types.FieldValue(cursor.ID.String()),
		)
	}

	// Add sorting
	sortOrderDesc := sortorder.Desc
	searchReq = searchReq.Sort(
		&types.SortOptions{
			SortOptions: map[string]types.FieldSort{
				"_score": {Order: &sortOrderDesc},
			},
		},
		&types.SortOptions{
			SortOptions: map[string]types.FieldSort{
				"id": {Order: &sortOrderDesc},
			},
		},
	)

	// Execute query
	res, err := searchReq.Do(ctx)
	if err != nil {
		slog.Error("Elasticsearch phrase query failed", "error", err, "query", query.Query, "fields", query.Fields)
		return nil, fmt.Errorf("failed to execute phrase search: %w", err)
	}

	page, err := searchResultPage(res.Hits, size)
	if err != nil {
		return nil, fmt.Errorf("failed to map search results to types: %w", err)
	}

	slog.Info("ES phrase search results fetched",
		"total_matches", page.TotalMatches,
		"returned_count", len(page.Hits),
		"max_score", page.MaxScore)

	return page, nil
}

func (r *Searcher) SearchBoolean(ctx context.Context, query *dquery.Boolean, baseOpts *dquery.BaseOptions) (*storage.SearchResult, error) {
	cursor, size := baseOpts.Cursor, baseOpts.Size

	slog.Info("Executing es boolean search",
		"expression", query.Expression,
		"has_cursor", cursor != nil,
		"size", size)

	tokens := r.tokenizer.Tokenize(query.Expression)
	if err := r.tokenizer.Validate(tokens); err != nil {
		slog.Error("Invalid boolean query expression", "error", err, "expression", query.Expression)
		return nil, fmt.Errorf("invalid boolean query expression: %w", err)
	}

	queryStringQuery := &types.QueryStringQuery{
		Query:  query.Expression,
		Fields: boostedFields(dquery.DefaultSearchContract().Fields),
	}

	searchReq := r.client.Search().
		Index(r.indexName).
		SourceExcludes_("embedding").
		Query(&types.Query{
			QueryString: queryStringQuery,
		}).
		Size(size + 1).
		TrackScores(true).
		TrackTotalHits(true)

	if cursor != nil {
		searchReq = searchReq.SearchAfter(
			types.FieldValue(cursor.Score),
			types.FieldValue(cursor.ID.String()),
		)
	}

	sortOrderDesc := sortorder.Desc
	searchReq = searchReq.Sort(
		&types.SortOptions{
			SortOptions: map[string]types.FieldSort{
				"_score": {Order: &sortOrderDesc},
			},
		},
		&types.SortOptions{
			SortOptions: map[string]types.FieldSort{
				"id": {Order: &sortOrderDesc},
			},
		},
	)

	res, err := searchReq.Do(ctx)
	if err != nil {
		slog.Error("Elasticsearch boolean query failed", "error", err, "expression", query.Expression)
		return nil, fmt.Errorf("failed to execute boolean search: %w", err)
	}

	page, err := searchResultPage(res.Hits, size)
	if err != nil {
		return nil, fmt.Errorf("failed to map search results to types: %w", err)
	}

	slog.Info("ES boolean search results fetched",
		"total_matches", page.TotalMatches,
		"returned_count", len(page.Hits),
		"max_score", page.MaxScore)

	return page, nil
}

// Compile-time interface assertions
var _ storage.FtsSearcher = (*Searcher)(nil)
