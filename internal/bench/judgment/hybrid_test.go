package judgment

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestHybridGradePool_FusesSignals(t *testing.T) {
	// lexicalHit shares query keywords but is semantically far;
	// semanticHit shares no keywords but is semantically close.
	// Both should land above NotRelev thanks to fusion.
	lexicalHit := GradingDoc{ID: uuid.New(), Title: "climate change policy summit", Description: "climate policy"}
	semanticHit := GradingDoc{ID: uuid.New(), Title: "global warming accord", Description: "emissions treaty"}
	cold := GradingDoc{ID: uuid.New(), Title: "celebrity gossip column", Description: "red carpet"}

	store := fakeVectorStore{
		query: []float32{1, 0},
		docs: map[uuid.UUID][]float32{
			lexicalHit.ID:  {0, 1},       // semantically far
			semanticHit.ID: {0.95, 0.31}, // cos ~0.95 -> strong semantic
			cold.ID:        {0, 1},       // far on both
		},
	}
	s := NewHybridStrategyWithStore(store, "fake")

	got, err := s.GradePool(context.Background(), GradingQuery{ID: "q", Text: "climate change policy"}, []GradingDoc{lexicalHit, semanticHit, cold})
	if err != nil {
		t.Fatalf("GradePool: %v", err)
	}
	byID := map[uuid.UUID]int{}
	for _, g := range got {
		byID[g.DocID] = g.Grade
	}
	if byID[lexicalHit.ID] <= GradeNotRelev {
		t.Errorf("lexical hit should score > 0 via BM25 component, got %d", byID[lexicalHit.ID])
	}
	if byID[semanticHit.ID] <= GradeNotRelev {
		t.Errorf("semantic hit should score > 0 via vector component, got %d", byID[semanticHit.ID])
	}
	if byID[cold.ID] != GradeNotRelev {
		t.Errorf("cold doc grade = %d, want %d", byID[cold.ID], GradeNotRelev)
	}
}

func TestHybridDescribe(t *testing.T) {
	s := NewHybridStrategyWithStore(fakeVectorStore{}, "qwen3")

	got := s.Describe()
	if got.Strategy != string(StrategyHybrid) {
		t.Errorf("Strategy = %q, want %q", got.Strategy, StrategyHybrid)
	}
	if got.Model != "bm25+qwen3" {
		t.Errorf("Model = %q, want %q", got.Model, "bm25+qwen3")
	}
}

// flakyVectorStore fails its first query embedding, the way an embedding
// endpoint times out on a cold model, and serves every later call.
type flakyVectorStore struct {
	fakeVectorStore
	calls atomic.Int32
}

func (f *flakyVectorStore) QueryVector(ctx context.Context, text string) ([]float32, error) {
	if f.calls.Add(1) == 1 {
		return nil, errors.New("embedding endpoint timed out")
	}
	return f.fakeVectorStore.QueryVector(ctx, text)
}

// A failed vector component leaves the pool unjudged. Grading each doc alone
// instead would normalise its BM25 score against itself and inflate it.
func TestHybrid_VectorFailureLeavesPoolUnjudged(t *testing.T) {
	pf, reader := bm25Pool("climate report", "climate summit on change", "economy and markets")
	store := &flakyVectorStore{fakeVectorStore: fakeVectorStore{query: []float32{1, 0}, docs: map[uuid.UUID][]float32{}}}
	for id := range reader.articles {
		store.docs[id] = []float32{0, 1}
	}

	grades := runGrades(t, RunnerConfig{Strategy: NewHybridStrategyWithStore(store, "fake"), Reader: reader}, pf)

	for id, grade := range grades {
		if grade != GradeUnjudged {
			t.Errorf("doc %q: grade %d, want unjudged", reader.articles[id].Title, grade)
		}
	}
}

func TestHybridGradePool_QueryWithoutTermsFails(t *testing.T) {
	doc := GradingDoc{ID: uuid.New(), Title: "anything"}
	store := fakeVectorStore{query: []float32{1, 0}, docs: map[uuid.UUID][]float32{doc.ID: {1, 0}}}

	_, err := NewHybridStrategyWithStore(store, "fake").GradePool(context.Background(), GradingQuery{ID: "q1", Text: "the a of"}, []GradingDoc{doc})

	var noTerms *NoQueryTermsError
	if !errors.As(err, &noTerms) || noTerms.QueryID != "q1" {
		t.Fatalf("GradePool error = %v, want a NoQueryTermsError for q1", err)
	}
}

func TestGradeFromHybridScore(t *testing.T) {
	cases := []struct {
		norm float64
		want int
	}{
		{1.0, GradeHighly},
		{0.66, GradeHighly},
		{0.5, GradeRelevant},
		{0.40, GradeRelevant},
		{0.2, GradeMarginally},
		{0.15, GradeMarginally},
		{0.1, GradeNotRelev},
		{0.0, GradeNotRelev},
	}
	for _, c := range cases {
		if got := gradeFromHybridScore(c.norm); got != c.want {
			t.Errorf("gradeFromHybridScore(%v) = %d, want %d", c.norm, got, c.want)
		}
	}
}
