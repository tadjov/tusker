package pg

import (
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
	"github.com/tadjov/tusker/internal/types/query"
)

func TestHybridSearcher_LexicalLegReadsTheQueryAsFreeText(t *testing.T) {
	pool := newEmbedTestPool(t)
	indexer, err := NewIndexer(pool)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	bothTerms, budgetOnly := uuid.New(), uuid.New()
	articles := []document.Article{
		{ID: bothTerms, Title: "budget tariff", URL: "https://example.com/1", Language: language.English},
		{ID: budgetOnly, Title: "budget", URL: "https://example.com/2", Language: language.English},
	}
	if err := indexer.SaveBulk(testCtx, articles); err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}
	// No article has an embedding, so the vector leg is empty and the hits are
	// exactly the lexical leg.
	searcher := NewHybridSearcher(embedding.NewEmbedder(staticEmbedder{vec: vec(1024, 0.9)}), pool)

	tests := []struct {
		name string
		text string
	}{
		{name: "a leading minus is not negation", text: "budget -tariff"},
		{name: "or is a stopword, not an operator", text: "budget or tariff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := searcher.SearchHybrid(testCtx, query.NewHybrid(tt.text), &query.BaseOptions{Size: 10})
			if err != nil {
				t.Fatalf("SearchHybrid: %v", err)
			}
			got := make([]uuid.UUID, 0, len(res.Hits))
			for _, hit := range res.Hits {
				got = append(got, hit.Article.ID)
			}
			if !slices.Equal(got, []uuid.UUID{bothTerms}) {
				t.Errorf("hits = %v, want only %s, the article holding every term", got, bothTerms)
			}
		})
	}
}
