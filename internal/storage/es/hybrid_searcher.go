package es

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/api/dto"
	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage"
	dquery "github.com/tadjov/tusker/internal/types/query"
	"github.com/tadjov/tusker/pkg/utils"
)

// hybridCandidateDepth bounds how many candidates each leg contributes to the fusion.
const hybridCandidateDepth = 200

type HybridSearcher struct {
	client    *elasticsearch.TypedClient
	indexName string
	embedder  *embedding.Embedder
	model     string
}

func NewHybridSearcher(client *Client, embedder *embedding.Embedder, model string) *HybridSearcher {
	return &HybridSearcher{
		client:    client.typed,
		indexName: client.indexName,
		embedder:  embedder,
		model:     model,
	}
}

// SearchHybrid runs a BM25 leg and a kNN leg over the same index and fuses them
// with Reciprocal Rank Fusion in Go. RRF scores are not a stable keyset, so the
// result is a single page (parity with the PG hybrid searcher).
func (s *HybridSearcher) SearchHybrid(ctx context.Context, query *dquery.Hybrid, baseOpts *dquery.BaseOptions) (*storage.SearchResult, error) {
	size := baseOpts.Size
	k := query.GetK()

	legDepth := hybridCandidateDepth
	if size > legDepth {
		legDepth = size
	}

	slog.Info("Executing es hybrid RRF search",
		"query", query.Query,
		"language", query.GetLanguage(),
		"k", k,
		"size", size,
		"leg_depth", legDepth)

	lexicalIDs, err := s.lexicalLeg(ctx, query.Query, legDepth)
	if err != nil {
		return nil, fmt.Errorf("hybrid lexical leg: %w", err)
	}

	vectorHits, err := s.vectorLeg(ctx, query.Query, legDepth)
	if err != nil {
		return nil, fmt.Errorf("hybrid vector leg: %w", err)
	}

	fused := fuseRRF(lexicalIDs, vectorHits, k)
	if len(fused) == 0 {
		return &storage.SearchResult{}, nil
	}

	totalMatches := int64(len(fused))
	if len(fused) > size {
		fused = fused[:size]
	}

	docs, err := s.fetchDocs(ctx, fused)
	if err != nil {
		return nil, fmt.Errorf("hybrid fetch docs: %w", err)
	}

	maxScore := fused[0].score
	articles := make([]dto.ArticleSearchResult, 0, len(fused))
	for _, c := range fused {
		doc, ok := docs[c.id]
		if !ok {
			continue
		}
		articles = append(articles, dto.ArticleSearchResult{
			Article:         doc,
			Score:           utils.RoundFloat64(c.score, dquery.ScoreDecimalPlaces),
			ScoreNormalized: utils.RoundFloat64(c.score/maxScore, dquery.ScoreDecimalPlaces),
		})
	}

	if len(articles) == 0 {
		return &storage.SearchResult{}, nil
	}

	slog.Info("ES hybrid search results fetched",
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

// lexicalLeg runs a BM25 multi_match over the default fields/weights and returns
// matched doc IDs in rank order.
func (s *HybridSearcher) lexicalLeg(ctx context.Context, query string, depth int) ([]uuid.UUID, error) {
	fields := dquery.DefaultFields
	weights := dquery.DefaultFieldWeights
	fieldsWithBoost := make([]string, 0, len(fields))
	for _, field := range fields {
		if w := weights[field]; w != 1.0 {
			fieldsWithBoost = append(fieldsWithBoost, fmt.Sprintf("%s^%.1f", field, w))
		} else {
			fieldsWithBoost = append(fieldsWithBoost, field)
		}
	}

	res, err := s.client.Search().
		Index(s.indexName).
		Query(&types.Query{
			MultiMatch: &types.MultiMatchQuery{Query: query, Fields: fieldsWithBoost},
		}).
		SourceIncludes_("id").
		Size(depth).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to execute lexical search: %w", err)
	}

	return idsFromHits(res.Hits.Hits)
}

// vectorLeg embeds the query and runs a kNN search over the vectors tagged with
// the query's model, returning matched doc IDs in rank order.
func (s *HybridSearcher) vectorLeg(ctx context.Context, query string, depth int) ([]uuid.UUID, error) {
	vec, err := s.embedder.EmbedQuery(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	numCandidates := depth
	if numCandidates < minNumCandidates {
		numCandidates = minNumCandidates
	}
	knn := types.KnnSearch{
		Field:         "embedding",
		QueryVector:   vec.Embedding,
		K:             &depth,
		NumCandidates: &numCandidates,
		Filter: []types.Query{{
			Term: map[string]types.TermQuery{"embedding_model": {Value: vec.Model}},
		}},
	}

	res, err := s.client.Search().
		Index(s.indexName).
		Knn(knn).
		SourceIncludes_("id").
		Size(depth).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kNN search: %w", err)
	}

	return idsFromHits(res.Hits.Hits)
}

type fusedCandidate struct {
	id    uuid.UUID
	score float64
}

// fuseRRF combines two ranked ID lists by Reciprocal Rank Fusion:
// score(doc) = sum over legs of 1/(k + rank).
func fuseRRF(lexical, vector []uuid.UUID, k int) []fusedCandidate {
	scores := make(map[uuid.UUID]float64)
	order := make([]uuid.UUID, 0, len(lexical)+len(vector))

	add := func(ids []uuid.UUID) {
		for rank, id := range ids {
			if _, seen := scores[id]; !seen {
				order = append(order, id)
			}
			scores[id] += 1.0 / float64(k+rank+1)
		}
	}
	add(lexical)
	add(vector)

	fused := make([]fusedCandidate, 0, len(order))
	for _, id := range order {
		fused = append(fused, fusedCandidate{id: id, score: scores[id]})
	}
	sort.SliceStable(fused, func(i, j int) bool {
		if fused[i].score != fused[j].score {
			return fused[i].score > fused[j].score
		}
		return fused[i].id.String() > fused[j].id.String()
	})
	return fused
}

// fetchDocs loads the full source for the fused candidates in a single query.
func (s *HybridSearcher) fetchDocs(ctx context.Context, candidates []fusedCandidate) (map[uuid.UUID]dto.Article, error) {
	res, err := s.client.Search().
		Index(s.indexName).
		SourceExcludes_("embedding").
		Query(&types.Query{
			Terms: &types.TermsQuery{
				TermsQuery: map[string]types.TermsQueryField{
					"id": idStrings(candidates),
				},
			},
		}).
		Size(len(candidates)).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch hybrid docs: %w", err)
	}

	docs := make(map[uuid.UUID]dto.Article, len(res.Hits.Hits))
	for _, hit := range res.Hits.Hits {
		article, err := decodeArticle(hit.Source_)
		if err != nil {
			return nil, err
		}
		docs[article.ID] = article
	}
	return docs, nil
}

func idStrings(candidates []fusedCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.id.String())
	}
	return out
}

func idsFromHits(hits []types.Hit) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(hits))
	for _, hit := range hits {
		var doc struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(hit.Source_, &doc); err != nil {
			return nil, fmt.Errorf("failed to unmarshal hit id: %w", err)
		}
		id, err := uuid.Parse(doc.ID)
		if err != nil {
			return nil, fmt.Errorf("parse document id %q: %w", doc.ID, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

var _ storage.HybridSearcher = (*HybridSearcher)(nil)
