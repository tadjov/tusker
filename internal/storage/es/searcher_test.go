package es

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/storagetest"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
	dquery "github.com/tadjov/tusker/internal/types/query"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

type searchMethod struct {
	name   string
	search func(ctx context.Context, s *Searcher, term string, opts *dquery.BaseOptions) (*storage.SearchResult, error)
}

var searchMethods = []searchMethod{
	{
		name: "SearchStringQuery",
		search: func(ctx context.Context, s *Searcher, term string, opts *dquery.BaseOptions) (*storage.SearchResult, error) {
			return s.SearchStringQuery(ctx, &dquery.String{Query: term}, opts)
		},
	},
	{
		name: "SearchField",
		search: func(ctx context.Context, s *Searcher, term string, opts *dquery.BaseOptions) (*storage.SearchResult, error) {
			match, err := dquery.NewMatch(dquery.FieldTitle, term)
			if err != nil {
				return nil, err
			}
			return s.SearchField(ctx, match, opts)
		},
	},
	{
		name: "SearchFields",
		search: func(ctx context.Context, s *Searcher, term string, opts *dquery.BaseOptions) (*storage.SearchResult, error) {
			q, err := dquery.NewMultiMatchQuery(term, []string{"title", "content"})
			if err != nil {
				return nil, err
			}
			return s.SearchFields(ctx, q, opts)
		},
	},
	{
		name: "SearchPhrase",
		search: func(ctx context.Context, s *Searcher, term string, opts *dquery.BaseOptions) (*storage.SearchResult, error) {
			q, err := dquery.NewPhrase(term, []dquery.Field{dquery.FieldTitle})
			if err != nil {
				return nil, err
			}
			return s.SearchPhrase(ctx, q, opts)
		},
	},
	{
		name: "SearchBoolean",
		search: func(ctx context.Context, s *Searcher, term string, opts *dquery.BaseOptions) (*storage.SearchResult, error) {
			return s.SearchBoolean(ctx, &dquery.Boolean{Expression: term}, opts)
		},
	},
}

func newSearcherTestEnv(t *testing.T) (*Indexer, *Searcher) {
	t.Helper()

	container := pkgtesting.NewESContainer(context.Background(), t)
	cfg := ClientConfig{Addresses: []string{container.Address}, IndexName: "articles_searcher_test"}

	indexer, err := NewIndexer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	return indexer, NewSearcher(newTestClient(t, cfg))
}

func indexArticles(t *testing.T, indexer *Indexer, titles ...string) []uuid.UUID {
	t.Helper()

	ids := make([]uuid.UUID, 0, len(titles))
	for _, title := range titles {
		id, err := indexer.Save(context.Background(), document.Article{ID: uuid.New(), Title: title, Language: language.English})
		if err != nil {
			t.Fatalf("index %q: %v", title, err)
		}
		ids = append(ids, id)
	}
	refreshIndex(t, indexer)
	return ids
}

func refreshIndex(t *testing.T, indexer *Indexer) {
	t.Helper()
	if _, err := indexer.client.Indices.Refresh().Index(indexer.indexName).Do(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
}

func TestSearcher_NoMatchReturnsEmptyPage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker (testcontainers ES)")
	}
	ctx := context.Background()
	indexer, searcher := newSearcherTestEnv(t)
	indexArticles(t, indexer, "climate change policy", "unrelated cooking recipe")

	for _, method := range searchMethods {
		t.Run(method.name, func(t *testing.T) {
			res, err := method.search(ctx, searcher, "asdfqwerzxcv", &dquery.BaseOptions{Size: 10})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(res.Hits) != 0 {
				t.Errorf("hits = %d, want none", len(res.Hits))
			}
			if res.HasMore {
				t.Error("HasMore = true on an empty page")
			}
			if res.NextCursor != nil {
				t.Errorf("NextCursor = %+v, want nil", res.NextCursor)
			}
			if res.MaxScore != 0 || res.PageMaxScore != 0 {
				t.Errorf("MaxScore = %f, PageMaxScore = %f, want both 0", res.MaxScore, res.PageMaxScore)
			}
		})
	}
}

func TestSearcher_MatchPagesThroughEveryHit(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker (testcontainers ES)")
	}
	ctx := context.Background()
	indexer, searcher := newSearcherTestEnv(t)
	// Different title lengths give distinct BM25 scores, so a page's first and
	// last hit differ and the cursor and PageMaxScore can be told apart.
	matching := indexArticles(t, indexer,
		"climate",
		"climate change",
		"climate change policy",
		"climate change policy debate review",
	)
	indexArticles(t, indexer, "unrelated cooking recipe")

	const pageSize = 2
	for _, method := range searchMethods {
		t.Run(method.name, func(t *testing.T) {
			seen := map[uuid.UUID]bool{}
			opts := &dquery.BaseOptions{Size: pageSize}
			for page := 1; ; page++ {
				if page > len(matching)+2 {
					t.Fatalf("still paging after %d pages", page-1)
				}
				res, err := method.search(ctx, searcher, "climate", opts)
				if err != nil {
					t.Fatalf("page %d: %v", page, err)
				}
				if len(res.Hits) == 0 {
					t.Fatalf("page %d is empty", page)
				}

				bestScore, bestNormalized := 0.0, 0.0
				for _, hit := range res.Hits {
					if seen[hit.Article.ID] {
						t.Errorf("page %d repeats article %s", page, hit.Article.ID)
					}
					seen[hit.Article.ID] = true
					if hit.ScoreNormalized <= 0 || hit.ScoreNormalized > 1 {
						t.Errorf("page %d: ScoreNormalized = %f, want (0, 1]", page, hit.ScoreNormalized)
					}
					bestScore = max(bestScore, hit.Score)
					bestNormalized = max(bestNormalized, hit.ScoreNormalized)
				}
				if math.Abs(res.PageMaxScore-bestScore) > 1e-3 {
					t.Errorf("page %d: PageMaxScore = %f, want the page's best score %f", page, res.PageMaxScore, bestScore)
				}
				if page == 1 && math.Abs(bestNormalized-1) > 1e-9 {
					t.Errorf("first page best ScoreNormalized = %f, want 1", bestNormalized)
				}

				if !res.HasMore {
					if res.NextCursor != nil {
						t.Errorf("last page carries NextCursor %+v", res.NextCursor)
					}
					break
				}
				if res.NextCursor == nil {
					t.Fatalf("page %d: HasMore without a NextCursor", page)
				}
				opts = &dquery.BaseOptions{Size: pageSize, Cursor: res.NextCursor}
			}

			for _, id := range matching {
				if !seen[id] {
					t.Errorf("matching article %s never returned", id)
				}
			}
		})
	}
}

func TestSearcher_NonUUIDDocumentIDReturnsError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker (testcontainers ES)")
	}
	ctx := context.Background()
	indexer, searcher := newSearcherTestEnv(t)

	doc := map[string]any{"id": "not-a-uuid", "title": "climate change policy", "language": "eng"}
	if _, err := indexer.client.Index(indexer.indexName).Id("not-a-uuid").Document(doc).Do(ctx); err != nil {
		t.Fatalf("index raw document: %v", err)
	}
	refreshIndex(t, indexer)

	res, err := searcher.SearchStringQuery(ctx, &dquery.String{Query: "climate"}, &dquery.BaseOptions{Size: 10})
	if err == nil {
		t.Fatalf("search returned %+v, want an error for the non-UUID id", res)
	}
	if !strings.Contains(err.Error(), "not-a-uuid") {
		t.Errorf("error = %v, want it to name the non-UUID id", err)
	}
}

func TestSearcher_Parity(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker (testcontainers ES)")
	}
	indexer, searcher := newSearcherTestEnv(t)
	if err := indexer.SaveBulk(context.Background(), storagetest.ParityCorpus()); err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}
	refreshIndex(t, indexer)

	storagetest.RunParity(t, storagetest.Elasticsearch, searcher)
	storagetest.RunFieldIsolation(t, searcher)
}

func TestSearcher_TotalMatchesIsExactPastTenThousand(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker (testcontainers ES)")
	}
	ctx := context.Background()
	indexer, searcher := newSearcherTestEnv(t)

	// Elasticsearch stops counting at 10,000 unless asked to track every hit.
	const matching = 10_050
	articles := make([]document.Article, matching)
	for i := range articles {
		articles[i] = document.Article{ID: uuid.New(), Title: "climate", Language: language.English}
	}
	if err := indexer.SaveBulk(ctx, articles); err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}
	refreshIndex(t, indexer)

	for _, method := range searchMethods {
		t.Run(method.name, func(t *testing.T) {
			res, err := method.search(ctx, searcher, "climate", &dquery.BaseOptions{Size: 10})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if res.TotalMatches != matching {
				t.Errorf("TotalMatches = %d, want %d", res.TotalMatches, matching)
			}
		})
	}
}
