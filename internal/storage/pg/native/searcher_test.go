package native

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/pg"
	"github.com/tadjov/tusker/internal/storage/storagetest"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
	"github.com/tadjov/tusker/internal/types/operator"
	dquery "github.com/tadjov/tusker/internal/types/query"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
	"github.com/testcontainers/testcontainers-go"
)

var (
	testCtx  context.Context
	testPool *pg.ConnectionPool
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
	os.Exit(runWithContainer(m))
}

func runWithContainer(m *testing.M) int {
	testCtx = context.Background()
	container, err := pkgtesting.NewPGContainer(testCtx, pkgtesting.PGConfig{
		Database: "news_test_db",
		Username: "test",
		Password: "test",
	})
	if err != nil {
		panic(err)
	}
	defer func() { _ = testcontainers.TerminateContainer(container.Container) }()

	testPool, err = pg.NewConnectionPool(testCtx, pg.PoolConfig{ConnStr: container.ConnString})
	if err != nil {
		panic(err)
	}
	defer testPool.Close()

	return m.Run()
}

func newSearcher(t *testing.T) *Searcher {
	t.Helper()
	if testPool == nil {
		t.Skip("requires Docker (testcontainers Postgres)")
	}
	if _, err := testPool.GetConn().Exec(testCtx, "TRUNCATE TABLE articles CASCADE"); err != nil {
		t.Fatalf("truncate articles: %v", err)
	}
	searcher, err := NewReader(testPool)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return searcher
}

func hitIDs(t *testing.T, s *Searcher, query *dquery.String) []uuid.UUID {
	t.Helper()
	res, err := s.SearchStringQuery(testCtx, query, &dquery.BaseOptions{Size: 100})
	if err != nil {
		t.Fatalf("SearchStringQuery(%q): %v", query.Query, err)
	}
	ids := make([]uuid.UUID, 0, len(res.Hits))
	for _, hit := range res.Hits {
		ids = append(ids, hit.Article.ID)
	}
	if int64(len(ids)) != res.TotalMatches {
		t.Errorf("TotalMatches = %d, but the page holds all %d hits", res.TotalMatches, len(ids))
	}
	return ids
}

func TestSearcher_Parity(t *testing.T) {
	s := newSearcher(t)
	indexer, err := pg.NewIndexer(testPool)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	if err := indexer.SaveBulk(testCtx, storagetest.ParityCorpus()); err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}

	storagetest.RunParity(t, storagetest.Postgres, s)
	storagetest.RunFieldIsolation(t, s)
}

func TestSearcher_StringQueryRanksWithPostgresDefaultWeights(t *testing.T) {
	s := newSearcher(t)
	indexer, err := pg.NewIndexer(testPool)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	titleOnly := uuid.New()
	descriptionOnly := uuid.New()
	articles := []document.Article{
		{ID: titleOnly, Title: "budget", URL: "https://example.com/1", Language: language.English},
		{ID: descriptionOnly, Title: "harbor", Description: "budget", URL: "https://example.com/2", Language: language.English},
	}
	if err := indexer.SaveBulk(testCtx, articles); err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}

	res, err := s.SearchStringQuery(testCtx, dquery.NewQueryString("budget"), &dquery.BaseOptions{Size: 10})
	if err != nil {
		t.Fatalf("SearchStringQuery: %v", err)
	}
	scores := map[uuid.UUID]float64{}
	for _, hit := range res.Hits {
		scores[hit.Article.ID] = hit.Score
	}
	for _, id := range []uuid.UUID{titleOnly, descriptionOnly} {
		if _, ok := scores[id]; !ok {
			t.Fatalf("hits = %v, want both articles", scores)
		}
	}

	// Default weights give A 2.5x B; the contract's 3:2 boosts would give 1.5.
	if ratio := scores[titleOnly] / scores[descriptionOnly]; ratio < 2 || ratio > 3 {
		t.Errorf("title-only score / description-only score = %v, want the default weights' 2.5", ratio)
	}
}

func TestSearcher_SmallRelativeBoostStillRanks(t *testing.T) {
	s := newSearcher(t)
	indexer, err := pg.NewIndexer(testPool)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	// The ids order the two the wrong way round, so a tie on score shows.
	moreOften := uuid.MustParse("00000000-0000-0000-0000-0000000000a0")
	once := uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	articles := []document.Article{
		{ID: moreOften, Title: "harbor", Content: "budget budget budget", URL: "https://example.com/1", Language: language.English},
		{ID: once, Title: "harbor", Content: "budget", URL: "https://example.com/2", Language: language.English},
	}
	if err := indexer.SaveBulk(testCtx, articles); err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}

	q, err := dquery.NewMultiMatchQuery("budget", []string{"title^100000", "content"})
	if err != nil {
		t.Fatalf("NewMultiMatchQuery: %v", err)
	}
	res, err := s.SearchFields(testCtx, q, &dquery.BaseOptions{Size: 10})
	if err != nil {
		t.Fatalf("SearchFields: %v", err)
	}
	var got []uuid.UUID
	for _, hit := range res.Hits {
		got = append(got, hit.Article.ID)
	}
	if !slices.Equal(got, []uuid.UUID{moreOften, once}) {
		t.Errorf("hits = %v, want %s ranked above %s: content keeps a weight of 1/100000", got, moreOften, once)
	}
}

func TestSearcher_OrMatchKeepsAnAmpersandInsideALexeme(t *testing.T) {
	s := newSearcher(t)
	indexer, err := pg.NewIndexer(testPool)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	// Postgres parses a URL into the whole URL, its host and its path; the
	// path lexeme here, /path?a=1&b=2, is the only one the two URLs share.
	samePath := uuid.New()
	articles := []document.Article{
		{ID: samePath, Title: "harbor", Content: "see other.org/path?a=1&b=2", URL: "https://example.com/1", Language: language.English},
		{ID: uuid.New(), Title: "harbor", Content: "budget", URL: "https://example.com/2", Language: language.English},
	}
	if err := indexer.SaveBulk(testCtx, articles); err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}

	match, err := dquery.NewMatch(dquery.FieldContent, "example.com/path?a=1&b=2", dquery.WithMatchOperator(operator.Or))
	if err != nil {
		t.Fatalf("NewMatch: %v", err)
	}
	res, err := s.SearchField(testCtx, match, &dquery.BaseOptions{Size: 10})
	if err != nil {
		t.Fatalf("SearchField: %v", err)
	}
	var got []uuid.UUID
	for _, hit := range res.Hits {
		got = append(got, hit.Article.ID)
	}
	if !slices.Equal(got, []uuid.UUID{samePath}) {
		t.Errorf("hits = %v, want only %s, which shares the path lexeme", got, samePath)
	}
}

func TestSearcher_StringQueryOfOnlyStopwordsReturnsNoHits(t *testing.T) {
	s := newSearcher(t)
	articles := storagetest.ParityCorpus()
	indexer, err := pg.NewIndexer(testPool)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	if err := indexer.SaveBulk(testCtx, articles); err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}

	if got := hitIDs(t, s, dquery.NewQueryString("the")); len(got) != 0 {
		t.Errorf("hits = %v, want none", got)
	}
}

func TestSearcher_ScoreNormalizedIsAShareOfTheBestMatch(t *testing.T) {
	s := newSearcher(t)
	indexer, err := pg.NewIndexer(testPool)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	if err := indexer.SaveBulk(testCtx, storagetest.ParityCorpus()); err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}

	match, err := dquery.NewMatch(dquery.FieldTitle, "budget")
	if err != nil {
		t.Fatalf("NewMatch: %v", err)
	}
	res, err := s.SearchField(testCtx, match, &dquery.BaseOptions{Size: 10})
	if err != nil {
		t.Fatalf("SearchField: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits for a term several titles hold")
	}
	best := 0.0
	for _, hit := range res.Hits {
		if math.IsNaN(hit.ScoreNormalized) || hit.ScoreNormalized <= 0 || hit.ScoreNormalized > 1 {
			t.Errorf("article %s: ScoreNormalized = %v, want it in (0, 1]", hit.Article.ID, hit.ScoreNormalized)
		}
		best = max(best, hit.ScoreNormalized)
	}
	if best != 1 {
		t.Errorf("best ScoreNormalized = %v, want 1", best)
	}
}

func TestSearcher_CursorPastEveryMatchReturnsEmptyPage(t *testing.T) {
	s := newSearcher(t)
	indexer, err := pg.NewIndexer(testPool)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	articles := []document.Article{
		{ID: uuid.New(), Title: "climate change policy", URL: "https://example.com/1", Language: language.English},
		{ID: uuid.New(), Title: "climate change", URL: "https://example.com/2", Language: language.English},
	}
	if err := indexer.SaveBulk(testCtx, articles); err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}

	// Every match ranks above 0, so no row sorts after this cursor while the
	// match count stays positive.
	pastEveryMatch := &dquery.Cursor{Score: 0, ID: uuid.MustParse("00000000-0000-0000-0000-000000000001")}
	opts := &dquery.BaseOptions{Size: 10, Cursor: pastEveryMatch}

	methods := []struct {
		name   string
		search func() (*storage.SearchResult, error)
	}{
		{"SearchStringQuery", func() (*storage.SearchResult, error) {
			return s.SearchStringQuery(testCtx, dquery.NewQueryString("climate change"), opts)
		}},
		{"SearchField", func() (*storage.SearchResult, error) {
			match, err := dquery.NewMatch(dquery.FieldTitle, "climate change")
			if err != nil {
				return nil, err
			}
			return s.SearchField(testCtx, match, opts)
		}},
		{"SearchFields", func() (*storage.SearchResult, error) {
			q, err := dquery.NewMultiMatchQuery("climate change", []string{"title", "content"})
			if err != nil {
				return nil, err
			}
			return s.SearchFields(testCtx, q, opts)
		}},
		{"SearchPhrase exact", func() (*storage.SearchResult, error) {
			q, err := dquery.NewPhrase("climate change", []dquery.Field{dquery.FieldTitle})
			if err != nil {
				return nil, err
			}
			return s.SearchPhrase(testCtx, q, opts)
		}},
		{"SearchPhrase with slop", func() (*storage.SearchResult, error) {
			q, err := dquery.NewPhrase("climate policy", []dquery.Field{dquery.FieldTitle}, dquery.WithPhraseSlop(2))
			if err != nil {
				return nil, err
			}
			return s.SearchPhrase(testCtx, q, opts)
		}},
		{"SearchBoolean", func() (*storage.SearchResult, error) {
			return s.SearchBoolean(testCtx, &dquery.Boolean{Expression: "climate AND change"}, opts)
		}},
	}

	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			res, err := method.search()
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(res.Hits) != 0 {
				t.Errorf("hits = %d, want none past the last match", len(res.Hits))
			}
			if res.HasMore || res.NextCursor != nil {
				t.Errorf("HasMore = %v, NextCursor = %+v, want no further page", res.HasMore, res.NextCursor)
			}
		})
	}
}

func TestSearcher_PageQueryErrorNamesTheMethod(t *testing.T) {
	s := newSearcher(t)
	indexer, err := pg.NewIndexer(testPool)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	if err := indexer.SaveBulk(testCtx, []document.Article{{ID: uuid.New(), Title: "climate change", URL: "https://example.com/1", Language: language.English}}); err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}

	// The match count succeeds; the page query's LIMIT size+1 is negative.
	opts := &dquery.BaseOptions{Size: -2}
	methods := []struct {
		name   string
		search func() (*storage.SearchResult, error)
	}{
		{"SearchStringQuery", func() (*storage.SearchResult, error) {
			return s.SearchStringQuery(testCtx, dquery.NewQueryString("climate"), opts)
		}},
		{"SearchField", func() (*storage.SearchResult, error) {
			match, err := dquery.NewMatch(dquery.FieldTitle, "climate")
			if err != nil {
				return nil, err
			}
			return s.SearchField(testCtx, match, opts)
		}},
		{"SearchFields", func() (*storage.SearchResult, error) {
			q, err := dquery.NewMultiMatchQuery("climate", []string{"title", "content"})
			if err != nil {
				return nil, err
			}
			return s.SearchFields(testCtx, q, opts)
		}},
		{"SearchPhrase", func() (*storage.SearchResult, error) {
			q, err := dquery.NewPhrase("climate change", []dquery.Field{dquery.FieldTitle})
			if err != nil {
				return nil, err
			}
			return s.SearchPhrase(testCtx, q, opts)
		}},
		{"SearchBoolean", func() (*storage.SearchResult, error) {
			return s.SearchBoolean(testCtx, &dquery.Boolean{Expression: "climate"}, opts)
		}},
	}

	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			_, err := method.search()
			if err == nil {
				t.Fatal("search succeeded with a negative page size")
			}
			if !strings.Contains(err.Error(), method.name) {
				t.Errorf("error = %q, want it to name %s", err, method.name)
			}
		})
	}
}

func TestSearcher_PagesThroughEveryHit(t *testing.T) {
	s := newSearcher(t)
	indexer, err := pg.NewIndexer(testPool)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	var matching []uuid.UUID
	for i, title := range []string{"climate", "climate change", "climate change policy", "climate change policy debate review", "unrelated cooking recipe"} {
		id := uuid.New()
		if err := indexer.SaveBulk(testCtx, []document.Article{{ID: id, Title: title, URL: fmt.Sprintf("https://example.com/%d", i), Language: language.English}}); err != nil {
			t.Fatalf("SaveBulk: %v", err)
		}
		if strings.Contains(title, "climate") {
			matching = append(matching, id)
		}
	}

	methods := []struct {
		name   string
		search func(opts *dquery.BaseOptions) (*storage.SearchResult, error)
	}{
		{"SearchStringQuery", func(opts *dquery.BaseOptions) (*storage.SearchResult, error) {
			return s.SearchStringQuery(testCtx, dquery.NewQueryString("climate"), opts)
		}},
		{"SearchField", func(opts *dquery.BaseOptions) (*storage.SearchResult, error) {
			match, err := dquery.NewMatch(dquery.FieldTitle, "climate")
			if err != nil {
				return nil, err
			}
			return s.SearchField(testCtx, match, opts)
		}},
		{"SearchFields", func(opts *dquery.BaseOptions) (*storage.SearchResult, error) {
			q, err := dquery.NewMultiMatchQuery("climate", []string{"title", "content"})
			if err != nil {
				return nil, err
			}
			return s.SearchFields(testCtx, q, opts)
		}},
		{"SearchPhrase", func(opts *dquery.BaseOptions) (*storage.SearchResult, error) {
			q, err := dquery.NewPhrase("climate", []dquery.Field{dquery.FieldTitle})
			if err != nil {
				return nil, err
			}
			return s.SearchPhrase(testCtx, q, opts)
		}},
		{"SearchBoolean", func(opts *dquery.BaseOptions) (*storage.SearchResult, error) {
			return s.SearchBoolean(testCtx, &dquery.Boolean{Expression: "climate"}, opts)
		}},
	}

	const pageSize = 2
	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			seen := map[uuid.UUID]bool{}
			opts := &dquery.BaseOptions{Size: pageSize}
			for page := 1; ; page++ {
				if page > len(matching)+2 {
					t.Fatalf("still paging after %d pages", page-1)
				}
				res, err := method.search(opts)
				if err != nil {
					t.Fatalf("page %d: %v", page, err)
				}
				if len(res.Hits) == 0 {
					t.Fatalf("page %d is empty", page)
				}
				for _, hit := range res.Hits {
					if seen[hit.Article.ID] {
						t.Errorf("page %d repeats article %s", page, hit.Article.ID)
					}
					seen[hit.Article.ID] = true
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

func TestNormalizedScore(t *testing.T) {
	tests := []struct {
		name     string
		rawScore float64
		maxScore float64
		want     float64
	}{
		{name: "fraction of the max", rawScore: 0.5, maxScore: 2, want: 0.25},
		{name: "rounded to four places", rawScore: 1, maxScore: 3, want: 0.3333},
		{name: "the max itself", rawScore: 0.7, maxScore: 0.7, want: 1},
		{name: "every match ranks zero", rawScore: 0, maxScore: 0, want: 0},
		{name: "negative max", rawScore: 0.5, maxScore: -1, want: 0},
		{name: "NaN max", rawScore: 0.5, maxScore: math.NaN(), want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizedScore(tt.rawScore, tt.maxScore); got != tt.want {
				t.Errorf("normalizedScore(%v, %v) = %v, want %v", tt.rawScore, tt.maxScore, got, tt.want)
			}
		})
	}
}

func TestBooleanMatchWithNegationCanUseTheSearchVectorIndex(t *testing.T) {
	newSearcher(t)
	tsquery, err := NewBooleanParser().Parse("trump AND NOT biden")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	match := buildBooleanMatch(tsquery, language.English)

	// With sequential scans priced out, the planner still picks one when the
	// predicate cannot be answered from the GIN index.
	tx, err := testPool.GetConn().Begin(testCtx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(testCtx) }()
	if _, err := tx.Exec(testCtx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	rows, err := tx.Query(testCtx, "EXPLAIN SELECT id FROM articles WHERE "+match.where, match.arg)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "idx_articles_search_vector") {
		t.Errorf("plan does not use idx_articles_search_vector:\n%s", strings.Join(plan, "\n"))
	}
}
