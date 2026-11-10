package judgment

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/bench/pool"
	"github.com/tadjov/tusker/internal/types/document"
)

func TestBM25GradePool_RanksByRelevance(t *testing.T) {
	q := GradingQuery{ID: "q1", Description: "climate change policy"}
	strong := GradingDoc{ID: uuid.New(), Title: "Climate change policy reshapes energy", Description: "New climate policy targets emissions"}
	weak := GradingDoc{ID: uuid.New(), Title: "Climate mentioned once", Description: "A short note"}
	none := GradingDoc{ID: uuid.New(), Title: "Local football results", Description: "Match report"}

	got, err := BM25Strategy{}.GradePool(context.Background(), q, []GradingDoc{strong, weak, none})
	if err != nil {
		t.Fatalf("GradePool: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d grades, want 3", len(got))
	}
	byID := map[uuid.UUID]int{}
	for _, g := range got {
		byID[g.DocID] = g.Grade
	}
	if byID[strong.ID] <= byID[weak.ID] {
		t.Errorf("strong (%d) should outrank weak (%d)", byID[strong.ID], byID[weak.ID])
	}
	if byID[none.ID] != GradeNotRelev {
		t.Errorf("non-matching doc grade = %d, want %d", byID[none.ID], GradeNotRelev)
	}
}

// A query with no usable terms cannot rank anything; grading every doc 0 would
// pass for a judged pool in which nothing is relevant.
func TestBM25GradePool_QueryWithoutTermsFails(t *testing.T) {
	q := GradingQuery{ID: "q1", Description: "the a of"}
	doc := GradingDoc{ID: uuid.New(), Title: "anything"}

	_, err := BM25Strategy{}.GradePool(context.Background(), q, []GradingDoc{doc})

	var noTerms *NoQueryTermsError
	if !errors.As(err, &noTerms) || noTerms.QueryID != "q1" {
		t.Fatalf("GradePool error = %v, want a NoQueryTermsError for q1", err)
	}
}

func TestGradeFromNorm(t *testing.T) {
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
		if got := gradeFromNorm(c.norm); got != c.want {
			t.Errorf("gradeFromNorm(%v) = %d, want %d", c.norm, got, c.want)
		}
	}
}

// bm25Pool builds a one-query pool whose articles carry the given titles, in
// that order.
func bm25Pool(titles ...string) (*pool.PoolFile, *stubStorageReader) {
	reader := &stubStorageReader{articles: map[uuid.UUID]document.Article{}}
	entry := pool.PoolEntry{QueryID: "q1", QueryDesc: "climate change"}
	for _, title := range titles {
		id := uuid.New()
		reader.articles[id] = document.Article{ID: id, Title: title}
		entry.Docs = append(entry.Docs, pool.PooledDoc{DocID: id})
	}
	return &pool.PoolFile{Queries: []pool.PoolEntry{entry}}, reader
}

func runGrades(t *testing.T, cfg RunnerConfig, pf *pool.PoolFile) map[uuid.UUID]int {
	t.Helper()
	jf, err := NewRunner(cfg).Run(context.Background(), pf)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := map[uuid.UUID]int{}
	for _, d := range jf.Queries[0].Docs {
		out[d.DocID] = d.Grade
	}
	return out
}

// A resumed run grades the remaining docs against the whole pool, so it lands
// on the grades a fresh run gives them.
func TestBM25_ResumedRunMatchesFreshRun(t *testing.T) {
	pf, reader := bm25Pool(
		"climate change climate change climate",
		"climate report",
		"change of policy",
		"economy and markets",
		"climate summit on change",
		"weather update",
	)
	fresh := runGrades(t, RunnerConfig{Strategy: NewBM25Strategy(), Reader: reader}, pf)

	interrupted := Entry{QueryID: "q1"}
	for i, d := range pf.Queries[0].Docs {
		grade := GradeUnjudged
		if i == 0 || i == 4 {
			grade = fresh[d.DocID]
		}
		interrupted.Docs = append(interrupted.Docs, GradedDoc{DocID: d.DocID, Grade: grade})
	}
	resumed := runGrades(t, RunnerConfig{
		Strategy: NewBM25Strategy(),
		Reader:   reader,
		Existing: &File{Queries: []Entry{interrupted}},
	}, pf)

	for id, want := range fresh {
		if resumed[id] != want {
			t.Errorf("doc %q: resumed grade %d, fresh grade %d", reader.articles[id].Title, resumed[id], want)
		}
	}
}

// Pool order carries no meaning, so reversing it must not move a grade, however
// many docs the pool holds.
func TestBM25_GradesIgnorePoolOrder(t *testing.T) {
	filler := func(titles []string, n int) []string {
		for i := 0; i < n; i++ {
			titles = append(titles, "economy and markets")
		}
		return titles
	}
	titles := filler([]string{"climate change climate change climate"}, 300)
	titles = filler(append(titles, "climate report"), 300)
	pf, reader := bm25Pool(titles...)
	forward := runGrades(t, RunnerConfig{Strategy: NewBM25Strategy(), Reader: reader}, pf)

	docs := pf.Queries[0].Docs
	reversed := make([]pool.PooledDoc, len(docs))
	for i, d := range docs {
		reversed[len(docs)-1-i] = d
	}
	pf.Queries[0].Docs = reversed
	backward := runGrades(t, RunnerConfig{Strategy: NewBM25Strategy(), Reader: reader}, pf)

	for id, want := range forward {
		if backward[id] != want {
			t.Errorf("doc %q: reversed-pool grade %d, forward grade %d", reader.articles[id].Title, backward[id], want)
		}
	}
}

func TestBM25_RejectsBatchSize(t *testing.T) {
	pf, reader := bm25Pool("climate report", "weather update")

	_, err := NewRunner(RunnerConfig{Strategy: NewBM25Strategy(), Reader: reader, BatchSize: 1}).Run(context.Background(), pf)

	if err == nil {
		t.Fatal("Run with a batch size: want an error, got nil")
	}
}

// Grades from a run that could not read every pooled article were scaled to a
// smaller pool, so resuming once the article is back must regrade the query.
func TestBM25_ResumeAfterMissingArticleMatchesFreshRun(t *testing.T) {
	pf, reader := bm25Pool(
		"climate change climate change climate",
		"climate report",
		"change of policy",
	)
	fresh := runGrades(t, RunnerConfig{Strategy: NewBM25Strategy(), Reader: reader}, pf)

	strongest := pf.Queries[0].Docs[0].DocID
	missing := reader.articles[strongest]
	delete(reader.articles, strongest)
	partial, err := NewRunner(RunnerConfig{Strategy: NewBM25Strategy(), Reader: reader}).Run(context.Background(), pf)
	if err != nil {
		t.Fatalf("Run with a missing article: %v", err)
	}
	reader.articles[strongest] = missing

	resumed := runGrades(t, RunnerConfig{Strategy: NewBM25Strategy(), Reader: reader, Existing: partial}, pf)

	for id, want := range fresh {
		if resumed[id] != want {
			t.Errorf("doc %q: resumed grade %d, fresh grade %d", reader.articles[id].Title, resumed[id], want)
		}
	}
}
