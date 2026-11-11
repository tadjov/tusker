package judgment

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/metrics"
)

// fakeVectorStore returns canned vectors so the scoring logic can be tested
// with no embedding backend or database.
type fakeVectorStore struct {
	query []float32
	docs  map[uuid.UUID][]float32
	model string
}

func (f fakeVectorStore) Model() string {
	return f.model
}

func (f fakeVectorStore) QueryVector(context.Context, string) ([]float32, error) {
	return f.query, nil
}

func (f fakeVectorStore) DocVectors(_ context.Context, ids []uuid.UUID) (map[uuid.UUID][]float32, error) {
	out := make(map[uuid.UUID][]float32)
	for _, id := range ids {
		if v, ok := f.docs[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// angledDocs gives doc i the unit vector at i·5° from the query vector (1, 0),
// so its cosine falls as i grows. Ids are fixed so a tie breaks the same way
// on every run.
func angledDocs(n int) ([]GradingDoc, fakeVectorStore) {
	store := fakeVectorStore{query: []float32{1, 0}, docs: map[uuid.UUID][]float32{}}
	docs := make([]GradingDoc, n)
	for i := range n {
		id := uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
		angle := float64(i) * 5 * math.Pi / 180
		store.docs[id] = []float32{float32(math.Cos(angle)), float32(math.Sin(angle))}
		docs[i] = GradingDoc{ID: id}
	}
	return docs, store
}

func vectorGrades(t *testing.T, store fakeVectorStore, docs []GradingDoc) map[uuid.UUID]int {
	t.Helper()
	got, err := NewVectorStrategyWithStore(store, "fake-model").GradePool(context.Background(), GradingQuery{ID: "q", Text: "q"}, docs)
	if err != nil {
		t.Fatalf("GradePool: %v", err)
	}
	byID := map[uuid.UUID]int{}
	for _, g := range got {
		byID[g.DocID] = g.Grade
	}
	return byID
}

// The semantic track measures recall against exact search: the pool's ten
// docs nearest the query are relevant, every other pooled doc is not.
func TestVectorGradePool_GradesTheTenNearestDocsRelevantAndTheRestNot(t *testing.T) {
	docs, store := angledDocs(15)
	shuffled := slices.Clone(docs)
	rand.New(rand.NewPCG(1, 2)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	got := vectorGrades(t, store, shuffled)

	require.Len(t, got, 15)
	for i, d := range docs {
		want := GradeNotRelev
		if i < 10 {
			want = GradeHighly
		}
		assert.Equal(t, want, got[d.ID], "doc %d at %d°", i, i*5)
	}

	nearest := make([]uuid.UUID, 10)
	for i := range nearest {
		nearest[i] = docs[9-i].ID
	}
	assert.Equal(t, 1.0, metrics.RecallAtK(nearest, got, 10, 1), "the exact top ten in reverse order")
	nineAndAFar := append(slices.Clone(nearest[:9]), docs[14].ID)
	assert.InDelta(t, 0.9, metrics.RecallAtK(nineAndAFar, got, 10, 1), 1e-12)
}

func TestVectorGradePool_BreaksATieAtTheTenthDocByID(t *testing.T) {
	docs, store := angledDocs(11)
	store.docs[docs[10].ID] = store.docs[docs[9].ID]

	got := vectorGrades(t, store, docs)

	assert.Equal(t, GradeHighly, got[docs[9].ID], "the lower id of the tied pair")
	assert.Equal(t, GradeNotRelev, got[docs[10].ID])
}

func TestVectorGradePool_LeavesDocsWithoutAVectorUnjudged(t *testing.T) {
	docs, store := angledDocs(3)
	delete(store.docs, docs[1].ID)

	got := vectorGrades(t, store, docs)

	assert.NotContains(t, got, docs[1].ID)
	assert.Equal(t, GradeHighly, got[docs[0].ID])
	assert.Equal(t, GradeHighly, got[docs[2].ID])
}

// A batch sees only part of the pool, so its ten nearest are not the pool's.
func TestVector_RejectsBatchSize(t *testing.T) {
	pf, articles := buildPool(t, 15)
	ids := make(map[uuid.UUID]struct{}, len(articles))
	for id := range articles {
		ids[id] = struct{}{}
	}
	store := widthMismatchStore(t, ids, 2, 2)

	_, err := NewRunner(RunnerConfig{
		Strategy:  NewVectorStrategyWithStore(store, "fake-model"),
		Reader:    &stubStorageReader{articles: articles},
		BatchSize: 5,
	}).Run(context.Background(), pf)

	assert.Error(t, err)
}

func TestVectorDescribe(t *testing.T) {
	s := NewVectorStrategyWithStore(fakeVectorStore{}, "qwen3")

	got := s.Describe()
	if got.Strategy != string(StrategyVector) {
		t.Errorf("Strategy = %q, want %q", got.Strategy, StrategyVector)
	}
	if got.Model != "qwen3" {
		t.Errorf("Model = %q, want %q", got.Model, "qwen3")
	}
}

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0}, []float32{1, 0}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
		{"len mismatch", []float32{1, 0}, []float32{1}, 0},
		{"empty", nil, []float32{1}, 0},
		{"zero vector", []float32{0, 0}, []float32{1, 0}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cosine(c.a, c.b); got != c.want {
				t.Errorf("cosine = %v, want %v", got, c.want)
			}
		})
	}
}
