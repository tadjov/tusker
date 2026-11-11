package judgment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/bench/pool"
	"github.com/tadjov/tusker/internal/types/document"
)

func TestBM25GradePool_RanksByRelevance(t *testing.T) {
	q := GradingQuery{ID: "q1", Text: "climate change policy"}
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
	q := GradingQuery{ID: "q1", Text: "the a of"}
	doc := GradingDoc{ID: uuid.New(), Title: "anything"}

	_, err := BM25Strategy{}.GradePool(context.Background(), q, []GradingDoc{doc})

	var noTerms *NoQueryTermsError
	if !errors.As(err, &noTerms) || noTerms.QueryID != "q1" {
		t.Fatalf("GradePool error = %v, want a NoQueryTermsError for q1", err)
	}
}

// bm25Pool builds a one-query pool whose articles carry the given titles, in
// that order.
func bm25Pool(titles ...string) (*pool.PoolFile, *stubStorageReader) {
	reader := &stubStorageReader{articles: map[uuid.UUID]document.Article{}}
	entry := pool.PoolEntry{QueryID: "q1", QueryText: "climate change"}
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

func bm25Grades(t *testing.T, text string, docs []GradingDoc) map[uuid.UUID]int {
	t.Helper()
	graded, err := BM25Strategy{}.GradePool(context.Background(), GradingQuery{ID: "q1", Text: text}, docs)
	if err != nil {
		t.Fatalf("GradePool(%q): %v", text, err)
	}
	out := map[uuid.UUID]int{}
	for _, g := range graded {
		out[g.DocID] = g.Grade
	}
	return out
}

// bm25 stems query and document alike, so an inflected query term scores
// exactly as its stem does.
func TestBM25GradePool_InflectedQueryGradesAsItsStem(t *testing.T) {
	docs := []GradingDoc{
		{ID: uuid.New(), Title: "Election results certified", Content: "The election board met."},
		{ID: uuid.New(), Title: "Elections in three states"},
		{ID: uuid.New(), Title: "Local football results"},
	}

	plural := bm25Grades(t, "elections", docs)
	singular := bm25Grades(t, "election", docs)

	if plural[docs[0].ID] == GradeNotRelev {
		t.Errorf("%q graded %d for \"elections\", want it to match", docs[0].Title, plural[docs[0].ID])
	}
	for _, d := range docs {
		if plural[d.ID] != singular[d.ID] {
			t.Errorf("%q: grade %d for \"elections\", %d for \"election\"", d.Title, plural[d.ID], singular[d.ID])
		}
	}
}

func TestBM25GradePool_TwoLetterQueryTermCounts(t *testing.T) {
	us := GradingDoc{ID: uuid.New(), Title: "US economy slows"}
	uk := GradingDoc{ID: uuid.New(), Title: "UK economy slows"}

	got := bm25Grades(t, "US", []GradingDoc{us, uk})

	if got[us.ID] <= got[uk.ID] {
		t.Errorf("US doc graded %d, UK doc %d: want the doc holding the query term graded higher", got[us.ID], got[uk.ID])
	}
}

// Every doc is four terms long, so avgdl = 4 and each doc sits at average
// length; climate and change each occur in three of the five docs, so they
// share one idf. At average length a term occurring tf times scores
// idf·(k1+1)·tf/(tf+k1), and the norm divides by Σ (k1+1)·idf over the query's
// terms: every term once is 1/(1+1.5) = 0.40, every term twice is
// 2/(2+1.5) = 0.571, one of the two terms once is 0.40/2 = 0.20.
func TestBM25GradePool_WorkedThresholdCases(t *testing.T) {
	docs := []GradingDoc{
		{ID: uuid.New(), Title: "climate change alpha beta"},
		{ID: uuid.New(), Title: "climate climate change change"},
		{ID: uuid.New(), Title: "climate gamma delta epsilon"},
		{ID: uuid.New(), Title: "change zeta theta iota"},
		{ID: uuid.New(), Title: "omega sigma kappa lambda"},
	}
	want := map[string]int{
		"climate change alpha beta":     GradeRelevant,
		"climate climate change change": GradeHighly,
		"climate gamma delta epsilon":   GradeMarginally,
		"change zeta theta iota":        GradeMarginally,
		"omega sigma kappa lambda":      GradeNotRelev,
	}

	got := bm25Grades(t, "climate change", docs)

	for _, d := range docs {
		if got[d.ID] != want[d.Title] {
			t.Errorf("%q graded %d, want %d", d.Title, got[d.ID], want[d.Title])
		}
	}
}

// Every term once in a doc three quarters of average length (3 terms against
// avgdl 16/4 = 4) has the norm 1/(1+K) with K = 1.5·(0.25+0.75·0.75) = 1.219:
// 0.451, under the 0.50 grade 3 needs.
func TestBM25GradePool_EveryTermOnceInAShortDocGradesRelevant(t *testing.T) {
	short := GradingDoc{ID: uuid.New(), Title: "climate change alpha"}
	docs := []GradingDoc{
		short,
		{ID: uuid.New(), Title: "omega sigma kappa lambda"},
		{ID: uuid.New(), Title: "gamma delta epsilon zeta"},
		{ID: uuid.New(), Title: "theta iota beta rho mu"},
	}

	got := bm25Grades(t, "climate change", docs)

	if got[short.ID] != GradeRelevant {
		t.Errorf("%q graded %d, want %d", short.Title, got[short.ID], GradeRelevant)
	}
}

// In each case both pools hold the same N docs, the doc under test and a rival
// holding climate in a 100-term doc plus one-term docs without it, so N,
// df(climate) = 2 and avgdl are the same in both, and so is the doc's norm.
// Only how often the rival holds climate changes: once, which the doc under
// test ties or beats, or 80 times.
//
// A 100-term doc among 18 one-term docs: avgdl = 10.9, K = 10.70, norm 0.086,
// and the rival at 80 scores 10.3 times it. A 10-term doc among 8: avgdl = 11.8,
// K = 1.328, norm 0.430, grade 2, and pool-max normalisation would grade it 3
// beside the single-mention rival and 2 beside the other.
func TestBM25GradePool_GradeIgnoresHowFarTheBestDocOutscoresIt(t *testing.T) {
	filler := func(n int) string { return strings.TrimSpace(strings.Repeat("filler ", n)) }
	tests := []struct {
		name     string
		length   int
		shortDoc int
		want     int
	}{
		{name: "the best doc scores 10 times it", length: 100, shortDoc: 18, want: GradeNotRelev},
		{name: "a relevant doc", length: 10, shortDoc: 8, want: GradeRelevant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			underTest := GradingDoc{ID: uuid.New(), Title: "climate", Content: filler(tt.length - 1)}
			pool := func(rivalClimates int) []GradingDoc {
				rival := GradingDoc{
					ID:      uuid.New(),
					Title:   strings.TrimSpace(strings.Repeat("climate ", rivalClimates)),
					Content: filler(100 - rivalClimates),
				}
				docs := []GradingDoc{underTest, rival}
				for range tt.shortDoc {
					docs = append(docs, GradingDoc{ID: uuid.New(), Title: "sport"})
				}
				return docs
			}

			tied := bm25Grades(t, "climate", pool(1))[underTest.ID]
			outscored := bm25Grades(t, "climate", pool(80))[underTest.ID]

			if tied != tt.want || outscored != tt.want {
				t.Errorf("grade %d beside a rival holding climate once, %d beside one holding it 80 times; want %d both",
					tied, outscored, tt.want)
			}
		})
	}
}
