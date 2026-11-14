package pg

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
	"github.com/tadjov/tusker/internal/types/query"
)

type staticEmbedder struct {
	vec []float32
}

func (s staticEmbedder) Generate(_ context.Context, _ embedding.Request) (*embedding.Response, error) {
	return &embedding.Response{Embedding: s.vec}, nil
}

func TestSemanticSearcher_SearchSemantic_OnlySeesTheQueryModel(t *testing.T) {
	pool := newEmbedTestPool(t)

	queryVec := vec(1024, 0.9)
	nearQuery := vec(1024, 0.9)
	for i := len(nearQuery) / 2; i < len(nearQuery); i++ {
		nearQuery[i] = 0.5
	}

	indexer, err := NewIndexer(pool)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	sameModel, otherModel := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{sameModel, otherModel} {
		article := document.Article{ID: id, Title: id.String(), URL: "http://test.com/" + id.String(), Language: language.English}
		if _, err := indexer.Save(testCtx, article); err != nil {
			t.Fatalf("index article: %v", err)
		}
	}

	_, err = NewEmbedder(pool).SaveBulk(testCtx, []*embedding.Vec{
		{ID: sameModel, Model: "qwen3-embedding:0.6b@1024", Embedding: nearQuery},
		{ID: otherModel, Model: embedding.DefaultModel, Embedding: queryVec},
	})
	if err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}

	searcher := NewSemanticSearcher(embedding.NewEmbedder(staticEmbedder{vec: queryVec}), pool)
	res, err := searcher.SearchSemantic(testCtx, &query.Semantic{Query: "anything"}, &query.BaseOptions{Size: 1})
	if err != nil {
		t.Fatalf("SearchSemantic: %v", err)
	}

	got := make([]uuid.UUID, 0, len(res.Hits))
	for _, hit := range res.Hits {
		got = append(got, hit.ID)
	}
	if len(got) != 1 || got[0] != sameModel {
		t.Errorf("hits = %v, want only %s: a vector from another model must not be ranked, nor take a slot of the limit", got, sameModel)
	}
}
