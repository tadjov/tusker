package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DjordjeVuckovic/tusker/internal/bench/judgment"
	"github.com/DjordjeVuckovic/tusker/internal/bench/meta"
	"github.com/DjordjeVuckovic/tusker/internal/bench/pool"
	"github.com/DjordjeVuckovic/tusker/internal/storage"
	"github.com/DjordjeVuckovic/tusker/internal/types/document"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const retrievingEngine = "engine-zeta-retriever"

// gradeFixture is a track whose pool holds queries × docs pairs, each doc
// with its own title, plus the bm25 judge file graded against that pool.
type gradeFixture struct {
	track    string
	articles articleStore
	pool     *pool.PoolFile
	byTitle  map[string]pairKey
}

func newGradeFixture(t *testing.T, queries, docsPerQuery int) *gradeFixture {
	t.Helper()
	track := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(track, "trec"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(track, "spec.yaml"), []byte("schema_version: 1\nid: grade\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(track, "suite.yaml"), []byte("schema_version: 1\nid: grade\n"), 0o644))

	fx := &gradeFixture{track: track, articles: articleStore{}, byTitle: map[string]pairKey{}}
	fx.pool = &pool.PoolFile{Meta: meta.New("pool")}
	for q := range queries {
		entry := pool.PoolEntry{QueryID: fmt.Sprintf("q%02d", q), QueryDesc: fmt.Sprintf("query text %02d", q)}
		for d := range docsPerQuery {
			id := uuid.New()
			title := fmt.Sprintf("Story %s-%03d.", entry.QueryID, d)
			fx.articles[id] = document.Article{ID: id, Title: title}
			fx.byTitle[title] = pairKey{queryID: entry.QueryID, docID: id}
			entry.Docs = append(entry.Docs, pool.PooledDoc{DocID: id, Sources: []string{retrievingEngine}})
		}
		fx.pool.Queries = append(fx.pool.Queries, entry)
	}
	require.NoError(t, pool.WritePoolFile(fx.pool, filepath.Join(track, "trec", "pool.yaml")))
	fx.writeJudge(t, func(int) int { return judgment.GradeRelevant })
	return fx
}

// writeJudge grades the n-th pooled pair, in pool order, gradeOf(n).
func (fx *gradeFixture) writeJudge(t *testing.T, gradeOf func(n int) int) {
	t.Helper()
	judge := judgment.NewBM25Strategy().Describe()
	jf := &judgment.File{Meta: meta.Meta{Judge: &judge, PoolRunID: fx.pool.Meta.RunID}}
	n := 0
	for _, entry := range fx.pool.Queries {
		graded := judgment.Entry{QueryID: entry.QueryID}
		for _, doc := range entry.Docs {
			graded.Docs = append(graded.Docs, judgment.GradedDoc{DocID: doc.DocID, Grade: gradeOf(n)})
			n++
		}
		jf.Queries = append(jf.Queries, graded)
	}
	require.NoError(t, judgment.WriteFile(jf, filepath.Join(fx.track, "trec", "annotations.bm25.yaml")))
}

func (fx *gradeFixture) judgeGrades(t *testing.T) map[pairKey]int {
	t.Helper()
	return gradesOf(t, filepath.Join(fx.track, "trec", "annotations.bm25.yaml"))
}

func (fx *gradeFixture) handGrades(t *testing.T) map[pairKey]int {
	t.Helper()
	return gradesOf(t, filepath.Join(fx.track, "trec", "annotations.manual.yaml"))
}

func gradesOf(t *testing.T, path string) map[pairKey]int {
	t.Helper()
	jf, err := judgment.ReadFile(path)
	require.NoError(t, err)
	grades := map[pairKey]int{}
	for _, qe := range jf.Queries {
		for _, d := range qe.Docs {
			grades[pairKey{queryID: qe.QueryID, docID: d.DocID}] = d.Grade
		}
	}
	return grades
}

func (fx *gradeFixture) grade(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	return fx.gradeWithReader(t, fx.articles, stdin, args...)
}

func (fx *gradeFixture) gradeWithReader(t *testing.T, reader storage.Reader, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := newGradeCmdWithReader(func(*cobra.Command, string) (storage.Reader, error) { return reader, nil })
	var out bytes.Buffer
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{fx.track, "--against", "bm25"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// shown lists the pairs whose article the output displayed, in the order they
// first appeared.
func (fx *gradeFixture) shown(output string) []pairKey {
	type sighting struct {
		at   int
		pair pairKey
	}
	var seen []sighting
	for title, pair := range fx.byTitle {
		if at := strings.Index(output, title); at >= 0 {
			seen = append(seen, sighting{at: at, pair: pair})
		}
	}
	slices.SortFunc(seen, func(a, b sighting) int { return a.at - b.at })
	pairs := make([]pairKey, len(seen))
	for i, s := range seen {
		pairs[i] = s.pair
	}
	return pairs
}

func skipAll() string { return strings.Repeat("s\n", 500) }

func TestGrade_ScriptedSessionSavesGradesAndARerunResumesAtTheFirstUngradedPair(t *testing.T) {
	fx := newGradeFixture(t, 2, 3)

	out, err := fx.grade(t, "2\n3\nu\n1\ns\nq\n")
	require.NoError(t, err)

	shown := fx.shown(out)
	require.Len(t, shown, 4)
	got := fx.handGrades(t)
	assert.Equal(t, judgment.GradeRelevant, got[shown[0]])
	assert.Equal(t, judgment.GradeMarginally, got[shown[1]])
	assert.Equal(t, judgment.GradeUnjudged, got[shown[2]])
	assert.Equal(t, judgment.GradeUnjudged, got[shown[3]])

	rerun, err := fx.grade(t, "q\n")
	require.NoError(t, err)
	assert.Equal(t, []pairKey{shown[2]}, fx.shown(rerun))
}

func TestGrade_UndoReturnsTheLastGradeToUnjudged(t *testing.T) {
	fx := newGradeFixture(t, 1, 3)

	out, err := fx.grade(t, "2\nu\nq\n")
	require.NoError(t, err)

	assert.Equal(t, judgment.GradeUnjudged, fx.handGrades(t)[fx.shown(out)[0]])
}

func TestGrade_EndOfInputEndsTheSessionWithGradesSaved(t *testing.T) {
	fx := newGradeFixture(t, 1, 3)

	out, err := fx.grade(t, "3\n")

	require.NoError(t, err)
	assert.Equal(t, judgment.GradeHighly, fx.handGrades(t)[fx.shown(out)[0]])
}

func TestGrade_UnknownAnswerRepromptsForTheSamePair(t *testing.T) {
	fx := newGradeFixture(t, 1, 3)

	out, err := fx.grade(t, "7\nyes\nu\n0\nq\n")
	require.NoError(t, err)

	shown := fx.shown(out)
	require.Len(t, shown, 2)
	got := fx.handGrades(t)
	assert.Equal(t, judgment.GradeNotRelev, got[shown[0]])
	assert.Equal(t, judgment.GradeUnjudged, got[shown[1]])
}

// failingStore stops serving articles after the first, like a store that
// went away mid-session.
type failingStore struct {
	articleStore
	served int
}

func (s *failingStore) GetByIDs(ctx context.Context, ids []uuid.UUID) ([]document.Article, error) {
	if s.served > 0 {
		return nil, errors.New("connection lost")
	}
	s.served++
	return s.articleStore.GetByIDs(ctx, ids)
}

func TestGrade_GradeIsSavedBeforeTheNextPairShows(t *testing.T) {
	fx := newGradeFixture(t, 1, 3)

	out, err := fx.gradeWithReader(t, &failingStore{articleStore: fx.articles}, "3\n2\n")

	require.Error(t, err)
	shown := fx.shown(out)
	require.Len(t, shown, 1)
	assert.Equal(t, judgment.GradeHighly, fx.handGrades(t)[shown[0]])
}

func TestGrade_PairsOfOneQueryAreShownTogether(t *testing.T) {
	fx := newGradeFixture(t, 3, 4)

	out, err := fx.grade(t, skipAll())
	require.NoError(t, err)

	var queryRuns []string
	for _, pair := range fx.shown(out) {
		if len(queryRuns) == 0 || queryRuns[len(queryRuns)-1] != pair.queryID {
			queryRuns = append(queryRuns, pair.queryID)
		}
	}
	assert.ElementsMatch(t, []string{"q00", "q01", "q02"}, queryRuns)
}

func TestGrade_ScreenShowsTheQueryAndTheArticle(t *testing.T) {
	fx := newGradeFixture(t, 1, 1)
	for id, a := range fx.articles {
		a.Description = "Ministers met in Geneva."
		a.PublishedAt = time.Date(2024, 3, 5, 9, 0, 0, 0, time.UTC)
		a.PublisherDomain = "example.org"
		a.Metadata.PublisherName = "Example Times"
		a.Content = strings.Repeat("a", contentPreviewRunes) + "TAIL"
		fx.articles[id] = a
	}

	out, err := fx.grade(t, "q\n")
	require.NoError(t, err)

	for _, want := range []string{"query text 00", "Story q00-000.", "Example Times", "2024-03-05", "Ministers met in Geneva.", strings.Repeat("a", contentPreviewRunes)} {
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, "TAIL")
}

// A full session graded the same way must read the same whatever the judge
// said, down to its closing lines.
func TestGrade_SessionHidesTheJudgesGradeAndTheRetrievingEngines(t *testing.T) {
	fx := newGradeFixture(t, 2, 3)
	sessionAgainst := func(judgeGrade int) string {
		fx.writeJudge(t, func(int) int { return judgeGrade })
		require.NoError(t, os.RemoveAll(filepath.Join(fx.track, "trec", "annotations.manual.yaml")))
		out, err := fx.grade(t, strings.Repeat("1\n", 6))
		require.NoError(t, err)
		return out
	}

	againstZero := sessionAgainst(judgment.GradeNotRelev)
	againstThree := sessionAgainst(judgment.GradeHighly)

	assert.Equal(t, againstZero, againstThree)
	assert.NotContains(t, againstZero, retrievingEngine)
	assert.NotContains(t, againstZero, "κ")
}

func TestGrade_RefusesAJudgeFileFromAnotherPool(t *testing.T) {
	fx := newGradeFixture(t, 1, 2)
	fx.pool.Meta = meta.New("pool")
	require.NoError(t, pool.WritePoolFile(fx.pool, filepath.Join(fx.track, "trec", "pool.yaml")))

	_, err := fx.grade(t, "q\n")

	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(fx.track, "trec", "annotations.manual.yaml"))
}

func TestGrade_SameSeedDrawsTheSameSampleAndAnotherSeedADifferentOne(t *testing.T) {
	fx := newGradeFixture(t, 2, 20)
	fx.writeJudge(t, func(n int) int { return n % 4 })

	sample := func(seed string) []pairKey {
		out, err := fx.grade(t, skipAll(), "--sample", "8", "--seed", seed)
		require.NoError(t, err)
		return fx.shown(out)
	}
	first := sample("1")

	require.Len(t, first, 8)
	assert.Equal(t, first, sample("1"))
	assert.NotElementsMatch(t, first, sample("2"))
}

func TestGrade_WalkDoesNotDependOnPoolOrder(t *testing.T) {
	fx := newGradeFixture(t, 3, 12)
	fx.writeJudge(t, func(n int) int { return n % 4 })
	walk := func() []pairKey {
		out, err := fx.grade(t, skipAll(), "--sample", "16")
		require.NoError(t, err)
		return fx.shown(out)
	}
	before := walk()

	slices.Reverse(fx.pool.Queries)
	for i := range fx.pool.Queries {
		slices.Reverse(fx.pool.Queries[i].Docs)
	}
	require.NoError(t, pool.WritePoolFile(fx.pool, filepath.Join(fx.track, "trec", "pool.yaml")))

	assert.Equal(t, before, walk())
}

func TestGrade_SampleTakesAnEqualShareOfEachJudgeGrade(t *testing.T) {
	fx := newGradeFixture(t, 2, 20)
	fx.writeJudge(t, func(n int) int { return max(0, n%8-4) })

	out, err := fx.grade(t, skipAll(), "--sample", "12")
	require.NoError(t, err)

	assert.Equal(t, map[int]int{0: 3, 1: 3, 2: 3, 3: 3}, gradeCounts(fx.shown(out), fx.judgeGrades(t)))
}

func TestGrade_SampleTakesAShortGradeWholeAndFillsFromTheOthers(t *testing.T) {
	fx := newGradeFixture(t, 2, 20)
	fx.writeJudge(t, func(n int) int {
		if n == 0 {
			return judgment.GradeHighly
		}
		return n % 3
	})

	out, err := fx.grade(t, skipAll(), "--sample", "12")
	require.NoError(t, err)

	shown := fx.shown(out)
	counts := gradeCounts(shown, fx.judgeGrades(t))
	assert.Len(t, shown, 12)
	assert.Equal(t, 1, counts[judgment.GradeHighly])
	for _, grade := range []int{0, 1, 2} {
		assert.GreaterOrEqual(t, counts[grade], 3, "judge grade %d", grade)
	}
}

func TestGrade_ResumedSampleSkipsHandGradedPairsAndKeepsTheRest(t *testing.T) {
	fx := newGradeFixture(t, 2, 20)
	fx.writeJudge(t, func(n int) int { return n % 4 })
	out, err := fx.grade(t, "1\nq\n", "--sample", "8")
	require.NoError(t, err)
	handGraded := fx.shown(out)[0]

	rerun, err := fx.grade(t, skipAll(), "--sample", "8")
	require.NoError(t, err)

	rest := fx.shown(rerun)

	assert.Len(t, rest, 7)
	assert.NotContains(t, rest, handGraded)
	assert.Equal(t, judgment.GradeMarginally, fx.handGrades(t)[handGraded])
}

func gradeCounts(pairs []pairKey, grades map[pairKey]int) map[int]int {
	counts := map[int]int{}
	for _, p := range pairs {
		counts[grades[p]]++
	}
	return counts
}

func (fx *gradeFixture) writeHandGrades(t *testing.T, gradeOf func(n int) int) {
	t.Helper()
	judge := judgment.NewManualStrategy().Describe()
	jf := &judgment.File{Meta: meta.Meta{Judge: &judge, PoolRunID: fx.pool.Meta.RunID}}
	n := 0
	for _, entry := range fx.pool.Queries {
		graded := judgment.Entry{QueryID: entry.QueryID}
		for _, doc := range entry.Docs {
			graded.Docs = append(graded.Docs, judgment.GradedDoc{DocID: doc.DocID, Grade: gradeOf(n)})
			n++
		}
		jf.Queries = append(jf.Queries, graded)
	}
	require.NoError(t, judgment.WriteFile(jf, filepath.Join(fx.track, "trec", "annotations.manual.yaml")))
}

func tableRow(t *testing.T, output, label string) []string {
	t.Helper()
	for line := range strings.SplitSeq(output, "\n") {
		if fields := strings.Fields(line); len(fields) > 2 && fields[0]+" "+fields[1] == label {
			return fields[2:]
		}
	}
	t.Fatalf("no %q row in:\n%s", label, output)
	return nil
}

// The expected κ is sklearn's cohen_kappa_score(hand, judge,
// weights="quadratic", labels=[0, 1, 2, 3]) over the six pairs both graded.
func TestGrade_ReportComparesThePairsGradedInBothFilesWithoutASession(t *testing.T) {
	fx := newGradeFixture(t, 1, 8)
	hand := []int{2, 1, 0, 3, 3, 1, 3, judgment.GradeUnjudged}
	judge := []int{2, 2, 0, 3, 1, 1, judgment.GradeUnjudged, 2}
	fx.writeJudge(t, func(n int) int { return judge[n] })
	fx.writeHandGrades(t, func(n int) int { return hand[n] })
	before := fx.handGrades(t)
	noStore := func(*cobra.Command, string) (storage.Reader, error) { return nil, errors.New("no store") }
	cmd := newGradeCmdWithReader(noStore)
	var out bytes.Buffer
	cmd.SetIn(strings.NewReader("0\n0\n0\n"))
	cmd.SetOut(&out)
	cmd.SetArgs([]string{fx.track, "--against", "bm25", "--report"})

	require.NoError(t, cmd.ExecuteContext(context.Background()))

	report := out.String()
	assert.Contains(t, report, "6 pairs")
	assert.Contains(t, report, "66.7%")
	assert.Contains(t, report, "0.615")
	assert.Equal(t, []string{"0", "1", "0", "1"}, tableRow(t, report, "hand 3"))
	assert.Equal(t, before, fx.handGrades(t))
}

func TestGrade_ReportWithoutHandGradesFails(t *testing.T) {
	fx := newGradeFixture(t, 1, 2)

	_, err := fx.grade(t, "", "--report")

	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(fx.track, "trec", "annotations.manual.yaml"))
}

func TestGrade_SessionEndsWithProgressOnly(t *testing.T) {
	fx := newGradeFixture(t, 1, 3)

	partial, err := fx.grade(t, "2\n0\nq\n")
	require.NoError(t, err)
	complete, err := fx.grade(t, "1\n")
	require.NoError(t, err)

	assert.Contains(t, partial, "graded 2 of 3")
	assert.NotContains(t, partial, "--report")
	assert.Contains(t, complete, "--report")
}

// cancelAtPrompt cancels the session's context once the prompt has been shown
// n times, the way Ctrl-C lands while the grader is reading.
type cancelAtPrompt struct {
	mu      sync.Mutex
	out     bytes.Buffer
	n       int
	cancel  context.CancelFunc
	prompts int
}

func (w *cancelAtPrompt) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prompts += bytes.Count(p, []byte("q quit >"))
	if w.prompts >= w.n {
		w.cancel()
	}
	return w.out.Write(p)
}

func (w *cancelAtPrompt) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.String()
}

func TestGrade_InterruptAtThePromptEndsTheSessionWithGradesSaved(t *testing.T) {
	fx := newGradeFixture(t, 1, 3)
	stdin, typing := io.Pipe()
	t.Cleanup(func() { _ = typing.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &cancelAtPrompt{n: 2, cancel: cancel}
	cmd := newGradeCmdWithReader(func(*cobra.Command, string) (storage.Reader, error) { return fx.articles, nil })
	cmd.SetIn(stdin)
	cmd.SetOut(out)
	cmd.SetArgs([]string{fx.track, "--against", "bm25"})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	go func() { _, _ = io.WriteString(typing, "3\n") }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the session kept waiting for input after the interrupt")
	}
	shown := fx.shown(out.String())
	require.Len(t, shown, 2)
	assert.Equal(t, judgment.GradeHighly, fx.handGrades(t)[shown[0]])
	assert.Equal(t, judgment.GradeUnjudged, fx.handGrades(t)[shown[1]])
}
