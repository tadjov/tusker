package native

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tadjov/tusker/internal/api/dto"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/pg"
	"github.com/tadjov/tusker/internal/types/operator"
	dquery "github.com/tadjov/tusker/internal/types/query"
	"github.com/tadjov/tusker/pkg/utils"
)

type Searcher struct {
	db *pgxpool.Pool
}

var articleColumns = pg.ArticleColumnList("")

func NewReader(pool *pg.ConnectionPool) (*Searcher, error) {
	return &Searcher{db: pool.GetConn()}, nil
}

// Capabilities reports a field as isolated only when no other field shares its
// weight band; Postgres full-text search has no typo tolerance.
func (r *Searcher) Capabilities() dquery.Capabilities {
	return dquery.Capabilities{
		Engine:          "postgres",
		DefaultOperator: DefaultOperator,
		PhraseSlop:      true,
		Boosts:          true,
		Fields:          isolatedFields(),
	}
}

// SearchStringQuery matches the query's SearchContract against the weighted search_vector.
func (r *Searcher) SearchStringQuery(ctx context.Context, query *dquery.String, baseOpts *dquery.BaseOptions) (*storage.SearchResult, error) {
	cursor, size := baseOpts.Cursor, baseOpts.Size
	contract := query.Contract()
	op := operator.Resolve(contract.Operator, DefaultOperator)
	slog.Info("Executing pool query_string search",
		"query", query.Query,
		"fields", contract.Fields,
		"operator", op,
		"language", contract.Language,
		"has_cursor", cursor != nil,
		"size", size)

	whereClause := buildTsWhereClause(contract.Fields, contract.Language, op, 1)
	// The contract's boosts reach Elasticsearch only. Postgres ranks with its
	// default weights, as the benchmark's Postgres arms do.
	rankExpr := buildRankExpression(nil, contract.Language, op, 1)

	var globalMaxScore float64
	var count int64
	maxSQL := fmt.Sprintf(`
		SELECT COALESCE(MAX(%s), 0.0) as max_score, COUNT(*)
		FROM articles
		WHERE %s
	`, rankExpr, whereClause)
	if err := r.db.QueryRow(ctx, maxSQL, query.Query).Scan(&globalMaxScore, &count); err != nil {
		slog.Error("Failed to fetch global max score", "error", err)
		return nil, fmt.Errorf("cannot fetch global max score: %w", err)
	}
	if count == 0 {
		return &storage.SearchResult{}, nil
	}
	slog.Info("Computed global max score", "max_score", globalMaxScore, "total_matches", count)

	var searchSQL string
	var args []any

	if cursor == nil {
		searchSQL = fmt.Sprintf(`
			SELECT
				%s,
				%s as rank
			FROM articles
			WHERE %s
			ORDER BY rank DESC, id DESC
			LIMIT $2
		`, articleColumns, rankExpr, whereClause)
		args = []any{query.Query, size + 1}
	} else {
		searchSQL = fmt.Sprintf(`
			SELECT
				%s,
				%s as rank
			FROM articles
			WHERE %s
			  AND (%s, id) < ($2, $3)
			ORDER BY rank DESC, id DESC
			LIMIT $4
		`, articleColumns, rankExpr, whereClause, rankExpr)
		args = []any{query.Query, cursor.Score, cursor.ID, size + 1}
	}

	return r.fetchPage(ctx, pageQuery{
		method:       "SearchStringQuery",
		sql:          searchSQL,
		args:         args,
		size:         size,
		maxScore:     globalMaxScore,
		totalMatches: count,
	})
}

// SearchField implements storage.SingleMatchSearcher interface
// Performs single-field match query using PostgreSQL's tsvector
func (r *Searcher) SearchField(ctx context.Context, query *dquery.Match, baseOpts *dquery.BaseOptions) (*storage.SearchResult, error) {
	cursor, size := baseOpts.Cursor, baseOpts.Size
	lang := query.GetLanguage()
	op := operator.Resolve(query.Operator, DefaultOperator)
	slog.Info("Executing pool match search",
		"query", query.Query,
		"field", query.Field,
		"operator", op,
		"language", lang,
		"has_cursor", cursor != nil,
		"size", size)

	fieldBoosts := query.Params().Fields
	whereClause := buildTsWhereClause(fieldBoosts, lang, op, 1)
	rankExpr := buildRankExpression(fieldBoosts, lang, op, 1)

	slog.Debug("PostgreSQL match query components",
		"where", whereClause,
		"rank", rankExpr)

	// Get global max score and total count
	var globalMaxScore float64
	var count int64
	maxSQL := fmt.Sprintf(`
		SELECT COALESCE(MAX(%s), 0.0) as max_score, COUNT(*)
		FROM articles
		WHERE %s
	`, rankExpr, whereClause)

	if err := r.db.QueryRow(ctx, maxSQL, query.Query).Scan(&globalMaxScore, &count); err != nil {
		slog.Error("Failed to fetch global max score", "error", err)
		return nil, fmt.Errorf("cannot fetch global max score: %w", err)
	}
	if count == 0 {
		return &storage.SearchResult{}, nil
	}
	slog.Info("Computed global max score", "max_score", globalMaxScore, "total_matches", count)

	var searchSQL string
	var args []any

	if cursor == nil {
		searchSQL = fmt.Sprintf(`
			SELECT
				%s,
				%s as rank
			FROM articles
			WHERE %s
			ORDER BY rank DESC, id DESC
			LIMIT $2
		`, articleColumns, rankExpr, whereClause)
		args = []any{query.Query, size + 1}
	} else {
		searchSQL = fmt.Sprintf(`
			SELECT
				%s,
				%s as rank
			FROM articles
			WHERE %s
			  AND (%s, id) < ($2, $3)
			ORDER BY rank DESC, id DESC
			LIMIT $4
		`, articleColumns, rankExpr, whereClause, rankExpr)
		args = []any{query.Query, cursor.Score, cursor.ID, size + 1}
	}

	return r.fetchPage(ctx, pageQuery{
		method:       "SearchField",
		sql:          searchSQL,
		args:         args,
		size:         size,
		maxScore:     globalMaxScore,
		totalMatches: count,
	})
}

// SearchFields implements storage.MultiMatchSearcher interface
// Performs multi-field match query using PostgreSQL's weighted tsvector
func (r *Searcher) SearchFields(ctx context.Context, query *dquery.MultiMatch, baseOpts *dquery.BaseOptions) (*storage.SearchResult, error) {
	cursor, size := baseOpts.Cursor, baseOpts.Size
	lang := query.GetLanguage()
	op := operator.Resolve(query.Operator, DefaultOperator)

	fieldBoosts := query.Params().Fields

	slog.Info("Executing pool multi_match search",
		"query", query.Query,
		"fields", query.Fields,
		"operator", op,
		"language", lang,
		"has_cursor", cursor != nil,
		"size", size)

	whereClause := buildTsWhereClause(fieldBoosts, lang, op, 1)
	rankExpr := buildRankExpression(fieldBoosts, lang, op, 1)

	slog.Info("PostgreSQL multi_match query components",
		"where_clause", whereClause,
		"rank_expression", rankExpr,
		"field_boosts", fieldBoosts,
		"num_fields", len(fieldBoosts))

	// Get global max score and total count
	var globalMaxScore float64
	var count int64
	maxSQL := fmt.Sprintf(`
		SELECT COALESCE(MAX(%s), 0.0) as max_score, COUNT(*)
		FROM articles
		WHERE %s
	`, rankExpr, whereClause)

	if err := r.db.QueryRow(ctx, maxSQL, query.Query).Scan(&globalMaxScore, &count); err != nil {
		slog.Error("Failed to fetch global max score", "error", err)
		return nil, fmt.Errorf("cannot fetch global max score: %w", err)
	}
	if count == 0 {
		return &storage.SearchResult{}, nil
	}
	slog.Info("Computed global max score", "max_score", globalMaxScore, "total_matches", count)

	var searchSQL string
	var args []any

	if cursor == nil {
		searchSQL = fmt.Sprintf(`
			SELECT
				%s,
				%s as rank
			FROM articles
			WHERE %s
			ORDER BY rank DESC, id DESC
			LIMIT $2
		`, articleColumns, rankExpr, whereClause)
		args = []any{query.Query, size + 1}
	} else {
		searchSQL = fmt.Sprintf(`
			SELECT
				%s,
				%s as rank
			FROM articles
			WHERE %s
			  AND (%s, id) < ($2, $3)
			ORDER BY rank DESC, id DESC
			LIMIT $4
		`, articleColumns, rankExpr, whereClause, rankExpr)
		args = []any{query.Query, cursor.Score, cursor.ID, size + 1}
	}

	return r.fetchPage(ctx, pageQuery{
		method:       "SearchFields",
		sql:          searchSQL,
		args:         args,
		size:         size,
		maxScore:     globalMaxScore,
		totalMatches: count,
	})
}

// SearchPhrase matches the phrase within the requested fields. Slop 0 uses
// phraseto_tsquery; a larger slop ORs every allowed distance between the
// phrase's lexemes, bound as a ready tsquery so they are not stemmed again.
func (r *Searcher) SearchPhrase(ctx context.Context, query *dquery.Phrase, baseOpts *dquery.BaseOptions) (*storage.SearchResult, error) {
	cursor, size := baseOpts.Cursor, baseOpts.Size
	lang := query.GetLanguage()
	slop := query.GetSlop()

	slog.Info("Executing pool phrase search",
		"query", query.Query,
		"fields", query.Fields,
		"slop", slop,
		"language", lang,
		"has_cursor", cursor != nil,
		"size", size)

	regconfig := lang.Analysis().PostgresConfig
	phraseExpr := fmt.Sprintf("phraseto_tsquery('%s'::regconfig, $1)", regconfig)
	phraseArg := query.Query
	if slop > 0 {
		var plainQuery string
		lexemeSQL := fmt.Sprintf("SELECT plainto_tsquery('%s'::regconfig, $1)::text", regconfig)
		if err := r.db.QueryRow(ctx, lexemeSQL, query.Query).Scan(&plainQuery); err != nil {
			return nil, fmt.Errorf("SearchPhrase: failed to tokenize phrase: %w", err)
		}
		if lexemes := quotedLexemes(plainQuery); len(lexemes) >= 2 {
			phraseExpr = "$1::tsquery"
			phraseArg = buildPhraseSlopQuery(lexemes, slop)
		}
	}

	whereClause := buildPhraseFieldMatch(query.Fields, phraseExpr)
	rankExpr := fmt.Sprintf("ts_rank(search_vector, %s)", phraseExpr)

	slog.Debug("PostgreSQL phrase query components",
		"where", whereClause,
		"rank", rankExpr,
		"slop", slop)

	var globalMaxScore float64
	var count int64
	maxSQL := fmt.Sprintf(`
		SELECT COALESCE(MAX(%s), 0.0) as max_score, COUNT(*)
		FROM articles
		WHERE %s
	`, rankExpr, whereClause)
	if err := r.db.QueryRow(ctx, maxSQL, phraseArg).Scan(&globalMaxScore, &count); err != nil {
		slog.Error("Failed to fetch global max score", "error", err)
		return nil, fmt.Errorf("cannot fetch global max score: %w", err)
	}
	if count == 0 {
		return &storage.SearchResult{}, nil
	}
	slog.Info("Computed global max score", "max_score", globalMaxScore, "total_matches", count)

	var searchSQL string
	var args []any

	if cursor == nil {
		searchSQL = fmt.Sprintf(`
			SELECT
				%s,
				%s as rank
			FROM articles
			WHERE %s
			ORDER BY rank DESC, id DESC
			LIMIT $2
		`, articleColumns, rankExpr, whereClause)
		args = []any{phraseArg, size + 1}
	} else {
		searchSQL = fmt.Sprintf(`
			SELECT
				%s,
				%s as rank
			FROM articles
			WHERE %s
			  AND (%s, id) < ($2, $3)
			ORDER BY rank DESC, id DESC
			LIMIT $4
		`, articleColumns, rankExpr, whereClause, rankExpr)
		args = []any{phraseArg, cursor.Score, cursor.ID, size + 1}
	}

	return r.fetchPage(ctx, pageQuery{
		method:       "SearchPhrase",
		sql:          searchSQL,
		args:         args,
		size:         size,
		maxScore:     globalMaxScore,
		totalMatches: count,
	})
}

func (r *Searcher) SearchBoolean(ctx context.Context, query *dquery.Boolean, baseOpts *dquery.BaseOptions) (*storage.SearchResult, error) {
	cursor, size := baseOpts.Cursor, baseOpts.Size
	lang := query.GetLanguage()

	slog.Info("Executing pool boolean search",
		"expression", query.Expression,
		"language", lang,
		"has_cursor", cursor != nil,
		"size", size)

	boolParser := NewBooleanParser()
	tsqueryStr, err := boolParser.Parse(query.Expression)
	if err != nil {
		return nil, fmt.Errorf("failed to parse boolean expression: %w", err)
	}

	slog.Debug("Parsed boolean expression", "input", query.Expression, "tsquery", tsqueryStr)

	match := buildBooleanMatch(tsqueryStr, lang)
	whereClause, rankExpr := match.where, match.rank

	var globalMaxScore float64
	var count int64
	maxSQL := fmt.Sprintf(`
		SELECT COALESCE(MAX(%s), 0.0) as max_score, COUNT(*)
		FROM articles
		WHERE %s
	`, rankExpr, whereClause)

	if err := r.db.QueryRow(ctx, maxSQL, match.arg).Scan(&globalMaxScore, &count); err != nil {
		slog.Error("Failed to fetch global max score", "error", err)
		return nil, fmt.Errorf("cannot fetch global max score: %w", err)
	}
	if count == 0 {
		return &storage.SearchResult{}, nil
	}
	slog.Info("Computed global max score", "max_score", globalMaxScore, "total_matches", count)

	var searchSQL string
	var args []any

	if cursor == nil {
		searchSQL = fmt.Sprintf(`
			SELECT
				%s,
				%s as rank
			FROM articles
			WHERE %s
			ORDER BY rank DESC, id DESC
			LIMIT $2
		`, articleColumns, rankExpr, whereClause)
		args = []any{match.arg, size + 1}
	} else {
		searchSQL = fmt.Sprintf(`
			SELECT
				%s,
				%s as rank
			FROM articles
			WHERE %s
			  AND (%s, id) < ($2, $3)
			ORDER BY rank DESC, id DESC
			LIMIT $4
		`, articleColumns, rankExpr, whereClause, rankExpr)
		args = []any{match.arg, cursor.Score, cursor.ID, size + 1}
	}

	return r.fetchPage(ctx, pageQuery{
		method:       "SearchBoolean",
		sql:          searchSQL,
		args:         args,
		size:         size,
		maxScore:     globalMaxScore,
		totalMatches: count,
	})
}

// pageQuery is a page of a ranked search whose SQL fetches size+1 rows: the
// extra row only signals that another page exists. maxScore and totalMatches
// describe every match, not just this page.
type pageQuery struct {
	method       string
	sql          string
	args         []any
	size         int
	maxScore     float64
	totalMatches int64
}

// fetchPage returns an empty page when the cursor sits past every match, which
// the match count, computed without the cursor, cannot tell.
func (r *Searcher) fetchPage(ctx context.Context, q pageQuery) (*storage.SearchResult, error) {
	rows, err := r.db.Query(ctx, q.sql, q.args...)
	if err != nil {
		return nil, fmt.Errorf("%s: failed to execute page query: %w", q.method, err)
	}
	defer rows.Close()

	var hits []dto.ArticleSearchResult
	var rawScores []float64
	for rows.Next() {
		var rawScore float64
		article, err := pg.ScanArticle(rows, &rawScore)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", q.method, err)
		}
		hits = append(hits, dto.ArticleSearchResult{
			Article:         *article,
			Score:           utils.RoundFloat64(rawScore, dquery.ScoreDecimalPlaces),
			ScoreNormalized: normalizedScore(rawScore, q.maxScore),
		})
		rawScores = append(rawScores, rawScore)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: failed to read page rows: %w", q.method, err)
	}

	page := &storage.SearchResult{
		MaxScore:     utils.RoundFloat64(q.maxScore, dquery.ScoreDecimalPlaces),
		TotalMatches: q.totalMatches,
	}
	if len(hits) == 0 {
		return page, nil
	}

	page.HasMore = len(hits) > q.size
	if page.HasMore {
		hits, rawScores = hits[:q.size], rawScores[:q.size]
		page.NextCursor = &dquery.Cursor{
			Score: rawScores[len(rawScores)-1],
			ID:    hits[len(hits)-1].Article.ID,
		}
	}
	page.Hits = hits
	page.PageMaxScore = utils.RoundFloat64(rawScores[0], dquery.ScoreDecimalPlaces)

	slog.Info("PG search page fetched", "page_hits", len(hits), "total_matches", q.totalMatches)
	return page, nil
}

// normalizedScore is 0 when maxScore is not positive, NaN included, where the
// ratio would be NaN or out of range.
func normalizedScore(rawScore, maxScore float64) float64 {
	if !(maxScore > 0) {
		return 0
	}
	return utils.RoundFloat64(rawScore/maxScore, dquery.ScoreDecimalPlaces)
}

// Compile-time interface assertions
var _ storage.FtsSearcher = (*Searcher)(nil)
