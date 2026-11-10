// Package storagetest holds search cases every storage engine must pass, with
// the recall worked out by hand over a small fixed corpus.
package storagetest

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
	"github.com/tadjov/tusker/internal/types/operator"
	"github.com/tadjov/tusker/internal/types/query"
)

// Engine names the backend an engine-specific expectation belongs to.
type Engine string

const (
	Postgres      Engine = "postgres"
	Elasticsearch Engine = "elasticsearch"
)

// The corpus terms are budget, tariff and harbor: none is a stopword and each
// stems to itself, so recall does not depend on how an engine analyses text.
// The one exception, agreed, stems to agre, which a second stemming pass turns
// into agr: a phrase that stems its terms twice misses it.
var (
	bothInTitle            = uuid.MustParse("00000000-0000-0000-0000-00000000c001")
	titleAndContent        = uuid.MustParse("00000000-0000-0000-0000-00000000c002")
	descriptionAndContent  = uuid.MustParse("00000000-0000-0000-0000-00000000c003")
	tariffTitleAuthor      = uuid.MustParse("00000000-0000-0000-0000-00000000c004")
	bothOnlyInKeywords     = uuid.MustParse("00000000-0000-0000-0000-00000000c005")
	budgetTitleOnly        = uuid.MustParse("00000000-0000-0000-0000-00000000c006")
	unrelated              = uuid.MustParse("00000000-0000-0000-0000-00000000c007")
	budgetTitleSection     = uuid.MustParse("00000000-0000-0000-0000-00000000c008")
	descriptionThenContent = uuid.MustParse("00000000-0000-0000-0000-00000000c009")
	termsApartInTitle      = uuid.MustParse("00000000-0000-0000-0000-00000000c010")
	tariffDescriptionOnly  = uuid.MustParse("00000000-0000-0000-0000-00000000c011")
	agreedOneWordApart     = uuid.MustParse("00000000-0000-0000-0000-00000000c012")
	phraseInDescription    = uuid.MustParse("00000000-0000-0000-0000-00000000c013")
)

// ParityCorpus returns the articles every ParityCase searches.
func ParityCorpus() []document.Article {
	articles := []document.Article{
		{ID: bothInTitle, Title: "budget tariff"},
		{ID: titleAndContent, Title: "budget", Content: "tariff"},
		{ID: descriptionAndContent, Title: "harbor", Description: "tariff", Content: "budget"},
		{ID: tariffTitleAuthor, Title: "tariff", Metadata: document.Metadata{Authors: []string{"budget"}}},
		{ID: bothOnlyInKeywords, Title: "harbor", Metadata: document.Metadata{Keywords: []string{"budget", "tariff"}}},
		{ID: budgetTitleOnly, Title: "budget"},
		{ID: unrelated, Title: "harbor"},
		{ID: budgetTitleSection, Title: "budget", Metadata: document.Metadata{Section: "tariff"}},
		{ID: descriptionThenContent, Title: "harbor", Description: "budget", Content: "tariff"},
		{ID: termsApartInTitle, Title: "budget harbor tariff"},
		{ID: tariffDescriptionOnly, Title: "harbor", Description: "tariff"},
		{ID: agreedOneWordApart, Title: "harbor deal agreed"},
		{ID: phraseInDescription, Title: "harbor", Description: "budget tariff talks"},
	}
	for i := range articles {
		articles[i].URL = "https://example.com/" + articles[i].ID.String()
		articles[i].Language = language.English
	}
	return articles
}

// ParityCase is one search over ParityCorpus. Want is the recall every engine
// must return; WantByEngine replaces it where the request leaves a choice, such
// as the operator, to the engine.
type ParityCase struct {
	Name         string
	Search       func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error)
	Want         []uuid.UUID
	WantByEngine map[Engine][]uuid.UUID
}

// Recall of "budget tariff" in a field set, with every term or any term. The
// contract fields are title, description and content.
var (
	titleEveryTerm    = []uuid.UUID{bothInTitle, termsApartInTitle}
	titleAnyTerm      = []uuid.UUID{bothInTitle, titleAndContent, tariffTitleAuthor, budgetTitleOnly, budgetTitleSection, termsApartInTitle}
	contractEveryTerm = []uuid.UUID{
		bothInTitle, titleAndContent, descriptionAndContent, descriptionThenContent, termsApartInTitle, phraseInDescription,
	}
	contractAnyTerm = []uuid.UUID{
		bothInTitle, titleAndContent, descriptionAndContent, tariffTitleAuthor, budgetTitleOnly,
		budgetTitleSection, descriptionThenContent, termsApartInTitle, tariffDescriptionOnly, phraseInDescription,
	}
	titleOrContentEveryTerm = []uuid.UUID{bothInTitle, titleAndContent, termsApartInTitle}
	titleOrContentAnyTerm   = []uuid.UUID{
		bothInTitle, titleAndContent, descriptionAndContent, tariffTitleAuthor, budgetTitleOnly,
		budgetTitleSection, descriptionThenContent, termsApartInTitle,
	}
)

func ParityCases() []ParityCase {
	return []ParityCase{
		{
			Name: "query string under the engine default operator",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return s.SearchStringQuery(ctx, query.NewQueryString("budget tariff"), page())
			},
			WantByEngine: map[Engine][]uuid.UUID{Postgres: contractEveryTerm, Elasticsearch: contractAnyTerm},
		},
		{
			Name: "match with AND",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchField(ctx, s, query.FieldTitle, "budget tariff", query.WithMatchOperator(operator.And))
			},
			Want: titleEveryTerm,
		},
		{
			Name: "match with OR",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchField(ctx, s, query.FieldTitle, "budget tariff", query.WithMatchOperator(operator.Or))
			},
			Want: titleAnyTerm,
		},
		{
			Name: "match with OR reads a leading minus as text",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchField(ctx, s, query.FieldTitle, "budget -tariff", query.WithMatchOperator(operator.Or))
			},
			Want: titleAnyTerm,
		},
		{
			Name: "match with OR of words no article holds",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchField(ctx, s, query.FieldTitle, "the of", query.WithMatchOperator(operator.Or))
			},
			Want: nil,
		},
		{
			Name: "match under the engine default operator",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchField(ctx, s, query.FieldTitle, "budget tariff")
			},
			WantByEngine: map[Engine][]uuid.UUID{Postgres: titleEveryTerm, Elasticsearch: titleAnyTerm},
		},
		{
			Name: "multi_match with AND matches terms across the fields",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchFields(ctx, s, []string{"title", "content"}, query.WithMultiMatchOperator(operator.And))
			},
			Want: titleOrContentEveryTerm,
		},
		{
			Name: "multi_match boosts leave the recall alone",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchFields(ctx, s, []string{"description^2.5", "content"}, query.WithMultiMatchOperator(operator.And))
			},
			Want: []uuid.UUID{descriptionAndContent, descriptionThenContent, phraseInDescription},
		},
		{
			Name: "multi_match with OR",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchFields(ctx, s, []string{"title", "content"}, query.WithMultiMatchOperator(operator.Or))
			},
			Want: titleOrContentAnyTerm,
		},
		{
			Name: "multi_match under the engine default operator",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchFields(ctx, s, []string{"title", "content"})
			},
			WantByEngine: map[Engine][]uuid.UUID{Postgres: titleOrContentEveryTerm, Elasticsearch: titleOrContentAnyTerm},
		},
		{
			Name: "phrase in one field",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchPhrase(ctx, s, "budget tariff", []query.Field{query.FieldTitle}, 0)
			},
			Want: []uuid.UUID{bothInTitle},
		},
		{
			// descriptionThenContent ends its description with budget and opens
			// its content with tariff.
			Name: "phrase restricts every term to the field",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchPhrase(ctx, s, "budget tariff", []query.Field{query.FieldContent}, 0)
			},
			Want: nil,
		},
		{
			Name: "phrase with slop restricts every term to the field",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchPhrase(ctx, s, "budget tariff", []query.Field{query.FieldContent}, 1)
			},
			Want: nil,
		},
		{
			// descriptionThenContent's phrase spans the end of one field and
			// the start of the next; only phraseInDescription holds it in one.
			Name: "phrase over several fields matches within each field",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchPhrase(ctx, s, "budget tariff", []query.Field{query.FieldDescription, query.FieldContent}, 0)
			},
			Want: []uuid.UUID{phraseInDescription},
		},
		{
			Name: "phrase with slop over several fields matches within each field",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchPhrase(ctx, s, "budget tariff", []query.Field{query.FieldDescription, query.FieldContent}, 1)
			},
			Want: []uuid.UUID{phraseInDescription},
		},
		{
			Name: "phrase with slop allows a word between the terms",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchPhrase(ctx, s, "budget tariff", []query.Field{query.FieldTitle}, 1)
			},
			Want: []uuid.UUID{bothInTitle, termsApartInTitle},
		},
		{
			Name: "phrase with slop stems each term once",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return searchPhrase(ctx, s, "harbor agreed", []query.Field{query.FieldTitle}, 1)
			},
			Want: []uuid.UUID{agreedOneWordApart},
		},
		{
			Name: "boolean AND across the contract fields",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return s.SearchBoolean(ctx, &query.Boolean{Expression: "budget AND tariff"}, page())
			},
			Want: contractEveryTerm,
		},
		{
			Name: "boolean OR across the contract fields",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return s.SearchBoolean(ctx, &query.Boolean{Expression: "budget OR tariff"}, page())
			},
			Want: contractAnyTerm,
		},
		{
			// A tariff outside the contract fields does not exclude an article.
			Name: "boolean NOT across the contract fields",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return s.SearchBoolean(ctx, &query.Boolean{Expression: "budget AND NOT tariff"}, page())
			},
			Want: []uuid.UUID{budgetTitleOnly, budgetTitleSection},
		},
		{
			Name: "boolean adjacent words under the engine default operator",
			Search: func(ctx context.Context, s storage.FtsSearcher) (*storage.SearchResult, error) {
				return s.SearchBoolean(ctx, &query.Boolean{Expression: "budget tariff"}, page())
			},
			WantByEngine: map[Engine][]uuid.UUID{Postgres: contractEveryTerm, Elasticsearch: contractAnyTerm},
		},
	}
}

func page() *query.BaseOptions {
	return &query.BaseOptions{Size: 100}
}

func searchFields(ctx context.Context, s storage.FtsSearcher, fields []string, opts ...query.MultiMatchQueryOption) (*storage.SearchResult, error) {
	q, err := query.NewMultiMatchQuery("budget tariff", fields, opts...)
	if err != nil {
		return nil, err
	}
	return s.SearchFields(ctx, q, page())
}

func searchField(ctx context.Context, s storage.FtsSearcher, field query.Field, text string, opts ...query.MatchQueryOption) (*storage.SearchResult, error) {
	q, err := query.NewMatch(field, text, opts...)
	if err != nil {
		return nil, err
	}
	return s.SearchField(ctx, q, page())
}

func searchPhrase(ctx context.Context, s storage.FtsSearcher, text string, fields []query.Field, slop int) (*storage.SearchResult, error) {
	q, err := query.NewPhrase(text, fields, query.WithPhraseSlop(slop))
	if err != nil {
		return nil, err
	}
	return s.SearchPhrase(ctx, q, page())
}

// RunParity runs every ParityCase on a searcher whose engine holds exactly
// ParityCorpus.
func RunParity(t *testing.T, engine Engine, searcher storage.FtsSearcher) {
	t.Helper()
	for _, c := range ParityCases() {
		t.Run(c.Name, func(t *testing.T) {
			want, ok := c.WantByEngine[engine]
			if !ok {
				want = c.Want
			}
			res, err := c.Search(t.Context(), searcher)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			got := make([]uuid.UUID, 0, len(res.Hits))
			for _, hit := range res.Hits {
				got = append(got, hit.Article.ID)
			}
			if !slices.Equal(sortedIDs(got), sortedIDs(want)) {
				t.Errorf("recall = %v, want %v", sortedIDs(got), sortedIDs(want))
			}
			if res.TotalMatches != int64(len(want)) {
				t.Errorf("TotalMatches = %d, want %d", res.TotalMatches, len(want))
			}
		})
	}
}

// RunFieldIsolation checks every field the searcher claims to isolate: a match
// on it returns exactly the articles whose own text in that field holds the term.
func RunFieldIsolation(t *testing.T, searcher storage.FtsSearcher) {
	t.Helper()
	const term = "budget"
	for _, field := range searcher.Capabilities().Fields {
		t.Run("match isolates "+string(field), func(t *testing.T) {
			var want []uuid.UUID
			for _, article := range ParityCorpus() {
				if strings.Contains(fieldText(article, field), term) {
					want = append(want, article.ID)
				}
			}
			res, err := searchField(t.Context(), searcher, field, term)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			got := make([]uuid.UUID, 0, len(res.Hits))
			for _, hit := range res.Hits {
				got = append(got, hit.Article.ID)
			}
			if !slices.Equal(sortedIDs(got), sortedIDs(want)) {
				t.Errorf("match on %s = %v, want %v", field, sortedIDs(got), sortedIDs(want))
			}
		})
	}
}

func fieldText(article document.Article, field query.Field) string {
	switch field {
	case query.FieldTitle:
		return article.Title
	case query.FieldDescription:
		return article.Description
	case query.FieldContent:
		return article.Content
	default:
		return ""
	}
}

func sortedIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return out
}
