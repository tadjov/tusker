package pg

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/tadjov/tusker/internal/api/dto"
	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage"
	dquery "github.com/tadjov/tusker/internal/types/query"
	"github.com/tadjov/tusker/pkg/utils"
)

// hybridCandidateDepth bounds how many candidates each leg contributes to the fusion.
const hybridCandidateDepth = 200

type HybridSearcher struct {
	embedder *embedding.Embedder
	db       *pgxpool.Pool
}

func NewHybridSearcher(embedder *embedding.Embedder, pool *ConnectionPool) *HybridSearcher {
	return &HybridSearcher{
		embedder: embedder,
		db:       pool.GetConn(),
	}
}

// SearchHybrid fuses a lexical FTS ranking with a vector ranking via RRF in SQL.
// The lexical leg reads the query as free text under Postgres' default AND.
// RRF fused scores are not a stable keyset, so hybrid returns a single page.
func (s *HybridSearcher) SearchHybrid(ctx context.Context, query *dquery.Hybrid, baseOpts *dquery.BaseOptions) (*storage.SearchResult, error) {
	size := baseOpts.Size
	lang := query.GetLanguage()
	k := query.GetK()

	slog.Info("Executing PG hybrid RRF search",
		"query", query.Query,
		"language", lang,
		"k", k,
		"size", size)

	vec, err := s.embedder.EmbedQuery(ctx, query.Query)
	if err != nil {
		return nil, fmt.Errorf("failed to embed hybrid query: %w", err)
	}
	vecEncoded := pgvector.NewVector(vec.Embedding)

	// Each leg must contribute at least size candidates so large pages can fill.
	legDepth := hybridCandidateDepth
	if size > legDepth {
		legDepth = size
	}

	cmd := fmt.Sprintf(`
		WITH lexical AS (
			SELECT a.id AS article_id,
				   ROW_NUMBER() OVER (
					   ORDER BY ts_rank(a.search_vector, plainto_tsquery('%[1]s'::regconfig, $1)) DESC, a.id DESC
				   ) AS lex_rank
			FROM articles a
			WHERE a.search_vector @@ plainto_tsquery('%[1]s'::regconfig, $1)
			ORDER BY lex_rank
			LIMIT $5
		),
		vector AS (
			SELECT e.article_id,
				   ROW_NUMBER() OVER (ORDER BY e.embedding <=> $2) AS vec_rank
			FROM article_embeddings e
			WHERE e.model_name = $3
			ORDER BY e.embedding <=> $2
			LIMIT $5
		),
		fused AS (
			SELECT COALESCE(l.article_id, v.article_id) AS article_id,
				   COALESCE(1.0 / ($4 + l.lex_rank), 0.0)
				 + COALESCE(1.0 / ($4 + v.vec_rank), 0.0) AS rrf_score
			FROM lexical l
			FULL OUTER JOIN vector v ON l.article_id = v.article_id
		)
		SELECT %[2]s, f.rrf_score,
			   COUNT(*) OVER () AS total_matches
		FROM fused f
		INNER JOIN articles a ON a.id = f.article_id
		ORDER BY f.rrf_score DESC, a.id DESC
		LIMIT $6
	`, lang.Analysis().PostgresConfig, ArticleColumnList("a"))

	rows, err := s.db.Query(
		ctx,
		cmd,
		query.Query,
		vecEncoded,
		vec.Model,
		k,
		legDepth,
		size,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to execute hybrid search query: %w", err)
	}
	defer rows.Close()

	var articles []dto.ArticleSearchResult
	var rawScores []float64
	var totalMatches int64

	for rows.Next() {
		var rawScore float64

		article, err := ScanArticle(rows, &rawScore, &totalMatches)
		if err != nil {
			return nil, err
		}

		articles = append(articles, dto.ArticleSearchResult{
			Article: *article,
			Score:   utils.RoundFloat64(rawScore, dquery.ScoreDecimalPlaces),
		})
		rawScores = append(rawScores, rawScore)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	if len(articles) == 0 {
		return &storage.SearchResult{}, nil
	}

	maxScore := rawScores[0]
	for i := range articles {
		articles[i].ScoreNormalized = utils.RoundFloat64(rawScores[i]/maxScore, dquery.ScoreDecimalPlaces)
	}

	slog.Info("PG hybrid search results fetched",
		"total_page_matches", len(articles),
		"total_matches", totalMatches,
		"max_score", maxScore)

	return &storage.SearchResult{
		Hits:         articles,
		NextCursor:   nil,
		HasMore:      false,
		MaxScore:     utils.RoundFloat64(maxScore, dquery.ScoreDecimalPlaces),
		PageMaxScore: utils.RoundFloat64(maxScore, dquery.ScoreDecimalPlaces),
		TotalMatches: totalMatches,
	}, nil
}

var _ storage.HybridSearcher = (*HybridSearcher)(nil)
