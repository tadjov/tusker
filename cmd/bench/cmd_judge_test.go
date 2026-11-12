package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/judgment"
	"github.com/tadjov/tusker/internal/bench/meta"
	"github.com/tadjov/tusker/internal/bench/pool"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/queryvectors"
	"github.com/tadjov/tusker/internal/types/document"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

// fakeStrategy implements judgment.Strategy so we can drive checkResumeCompat
// without a real LLM/CLI backend.
type fakeStrategy struct {
	judge meta.Judge
}

func (f fakeStrategy) Name() string         { return f.judge.Strategy }
func (f fakeStrategy) Describe() meta.Judge { return f.judge }
func (f fakeStrategy) Grade(context.Context, judgment.GradingQuery, judgment.GradingDoc) (int, error) {
	return 0, nil
}

func llmJudge(provider, model string) meta.Judge {
	return meta.Judge{
		Strategy:      string(judgment.StrategyLLM),
		Provider:      provider,
		Model:         model,
		PromptVersion: judgment.PromptVersion,
	}
}

func fileWith(j meta.Judge) *judgment.File {
	return &judgment.File{Meta: meta.Meta{Judge: &j}}
}

func TestCheckResumeCompat_FileWithoutJudgeResumes(t *testing.T) {
	// A file carrying no judge block at all has nothing to contradict.
	prior := &judgment.File{}
	strat := fakeStrategy{judge: llmJudge("claude-cli", judgment.DefaultJudgeModel)}

	err := checkResumeCompat(prior, strat)
	assert.NoError(t, err, "file with no judge block should be resumable")
}

func TestCheckResumeCompat_IdenticalJudgeMatches(t *testing.T) {
	j := llmJudge("claude-cli", judgment.DefaultJudgeModel)

	err := checkResumeCompat(fileWith(j), fakeStrategy{judge: j})
	assert.NoError(t, err)
}

func TestCheckResumeCompat_StrategyMismatch(t *testing.T) {
	prior := fileWith(meta.Judge{Strategy: string(judgment.StrategyKeyword)})
	strat := fakeStrategy{judge: llmJudge("claude-cli", judgment.DefaultJudgeModel)}

	err := checkResumeCompat(prior, strat)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "judge mismatch")
}

func TestCheckResumeCompat_ModelMismatch(t *testing.T) {
	prior := fileWith(llmJudge("claude-cli", "claude-haiku-4-5-20251001"))
	strat := fakeStrategy{judge: llmJudge("claude-cli", "claude-opus-5")}

	err := checkResumeCompat(prior, strat)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "judge mismatch")
}

// A provider swap at the same strategy and model must be caught too — the case
// the old flat judge_model field could not express.
func TestCheckResumeCompat_ProviderMismatch(t *testing.T) {
	prior := fileWith(llmJudge("claude-cli", "some-model"))
	strat := fakeStrategy{judge: llmJudge("codex-cli", "some-model")}

	err := checkResumeCompat(prior, strat)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "judge mismatch")
}

func TestCheckResumeCompat_PromptVersionMismatch(t *testing.T) {
	prior := fileWith(meta.Judge{
		Strategy:      string(judgment.StrategyLLM),
		Provider:      "claude-cli",
		Model:         judgment.DefaultJudgeModel,
		PromptVersion: "v0-old",
	})
	strat := fakeStrategy{judge: llmJudge("claude-cli", judgment.DefaultJudgeModel)}

	err := checkResumeCompat(prior, strat)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "judge mismatch")
}

// A CLI judge run with other flags reads other settings, so its grades are not
// the same judge's.
func TestCheckResumeCompat_InvocationMismatch(t *testing.T) {
	was := llmJudge("claude-cli", "claude-haiku-5-5")
	was.Invocation = "claude -p --model claude-haiku-5-5"
	now := was
	now.Invocation = "claude -p --safe-mode --model claude-haiku-5-5"

	err := checkResumeCompat(fileWith(was), fakeStrategy{judge: now})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "judge mismatch")
}

// Legacy annotations (flat strategy/judge_model, no judge block) normalize to a
// judge whose strategy is the old selector, which cannot equal the new
// llm/claude-cli shape — so resuming onto them is correctly refused. Those
// grades came from an unrecorded model and must not be extended.
func TestCheckResumeCompat_LegacyFileRefusesResume(t *testing.T) {
	prior := &judgment.File{Meta: meta.Meta{
		LegacyStrategy:   "claude-cli",
		LegacyJudgeModel: "claude",
	}}
	prior.Meta.Normalize()
	strat := fakeStrategy{judge: llmJudge("claude-cli", judgment.DefaultJudgeModel)}

	err := checkResumeCompat(prior, strat)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "judge mismatch")
}

func judgmentsWithGrades(grades ...int) *judgment.File {
	docs := make([]judgment.GradedDoc, len(grades))
	for i, g := range grades {
		docs[i] = judgment.GradedDoc{DocID: uuid.New(), Grade: g}
	}
	return &judgment.File{
		Meta:    meta.Meta{Judge: &meta.Judge{Strategy: string(judgment.StrategyBM25)}},
		Queries: []judgment.Entry{{QueryID: "q1", Docs: docs}},
	}
}

func TestRequireJudged(t *testing.T) {
	cases := []struct {
		name          string
		grades        []int
		allowUnjudged bool
		wantErr       bool
	}{
		{name: "every doc graded", grades: []int{0, 3}},
		{name: "an unjudged doc fails", grades: []int{2, judgment.GradeUnjudged}, wantErr: true},
		{name: "an unjudged doc is allowed on request", grades: []int{2, judgment.GradeUnjudged}, allowUnjudged: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireJudged(judgmentsWithGrades(tc.grades...), tc.allowUnjudged)

			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// A heuristic file written under another rubric must not be extended: its
// grades and this run's would follow different rules under one judge block.
func TestCheckResumeCompat_RubricVersionMismatch(t *testing.T) {
	prior := fileWith(meta.Judge{Strategy: string(judgment.StrategyBM25)})

	err := checkResumeCompat(prior, judgment.NewBM25Strategy())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "judge mismatch")
}

func TestBuildManualJudgments_KeepsHandGradesAndAddsNewPoolDocs(t *testing.T) {
	graded, placeholder, newInPool, leftThePool, newQueryDoc := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	pf := &pool.PoolFile{Queries: []pool.PoolEntry{
		{QueryID: "q1", Docs: []pool.PooledDoc{{DocID: graded}, {DocID: placeholder}, {DocID: newInPool}}},
		{QueryID: "q2", Docs: []pool.PooledDoc{{DocID: newQueryDoc}}},
	}}
	existing := &judgment.File{Queries: []judgment.Entry{{QueryID: "q1", Docs: []judgment.GradedDoc{
		{DocID: graded, Grade: judgment.GradeRelevant},
		{DocID: placeholder, Grade: judgment.GradeUnjudged},
		{DocID: leftThePool, Grade: judgment.GradeNotRelev},
	}}}}

	got := buildManualJudgments(pf, existing)

	want := map[string]map[uuid.UUID]int{
		"q1": {
			graded:      judgment.GradeRelevant,
			placeholder: judgment.GradeUnjudged,
			newInPool:   judgment.GradeUnjudged,
			leftThePool: judgment.GradeNotRelev,
		},
		"q2": {newQueryDoc: judgment.GradeUnjudged},
	}
	assert.Equal(t, want, gradesByQuery(got))
}

func TestBuildManualJudgments_WithoutExistingFileWritesPlaceholders(t *testing.T) {
	doc := uuid.New()
	pf := &pool.PoolFile{Queries: []pool.PoolEntry{{QueryID: "q1", Docs: []pool.PooledDoc{{DocID: doc}}}}}

	got := buildManualJudgments(pf, nil)

	assert.Equal(t, map[string]map[uuid.UUID]int{"q1": {doc: judgment.GradeUnjudged}}, gradesByQuery(got))
}

func gradesByQuery(jf *judgment.File) map[string]map[uuid.UUID]int {
	out := map[string]map[uuid.UUID]int{}
	for _, qe := range jf.Queries {
		out[qe.QueryID] = map[uuid.UUID]int{}
		for _, d := range qe.Docs {
			out[qe.QueryID][d.DocID] = d.Grade
		}
	}
	return out
}

type articleStore map[uuid.UUID]document.Article

func (s articleStore) GetByIDs(_ context.Context, ids []uuid.UUID) ([]document.Article, error) {
	out := make([]document.Article, 0, len(ids))
	for _, id := range ids {
		if a, ok := s[id]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// judgeFixture is a track on disk whose pool holds one query, plus the store
// its pooled articles are read from.
type judgeFixture struct {
	track    string
	articles articleStore
	pool     *pool.PoolFile
}

func newJudgeFixture(t *testing.T, queryText string, titles ...string) *judgeFixture {
	t.Helper()
	track := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(track, "trec"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(track, "spec.yaml"), []byte("schema_version: 1\nid: judge\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(track, "suite.yaml"), []byte("schema_version: 1\nid: judge\n"), 0o644))

	fx := &judgeFixture{track: track, articles: articleStore{}}
	entry := pool.PoolEntry{QueryID: "q1", QueryText: queryText}
	for _, title := range titles {
		id := uuid.New()
		fx.articles[id] = document.Article{ID: id, Title: title}
		entry.Docs = append(entry.Docs, pool.PooledDoc{DocID: id})
	}
	fx.pool = &pool.PoolFile{Meta: meta.New("pool"), Queries: []pool.PoolEntry{entry}}
	fx.writePool(t)
	return fx
}

func (fx *judgeFixture) writePool(t *testing.T) {
	t.Helper()
	require.NoError(t, pool.WritePoolFile(fx.pool, filepath.Join(fx.track, "trec", "pool.yaml")))
}

func (fx *judgeFixture) judge(t *testing.T, args ...string) error {
	t.Helper()
	_, err := fx.judgeOutput(t, args...)
	return err
}

func (fx *judgeFixture) judgeOutput(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newJudgeCmdWithReader(func(*cobra.Command, string) (storage.Reader, error) {
		return fx.articles, nil
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{fx.track}, args...))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func (fx *judgeFixture) writePriorBM25(t *testing.T, runID string, queries ...judgment.Entry) {
	t.Helper()
	judge := judgment.NewBM25Strategy().Describe()
	require.NoError(t, judgment.WriteFile(&judgment.File{
		Meta:    meta.Meta{RunID: runID, Judge: &judge, PoolRunID: fx.pool.Meta.RunID},
		Queries: queries,
	}, filepath.Join(fx.track, "trec", "annotations.bm25.yaml")))
}

func (fx *judgeFixture) annotations(t *testing.T, set string) *judgment.File {
	t.Helper()
	jf, err := judgment.ReadFile(filepath.Join(fx.track, "trec", "annotations."+set+".yaml"))
	require.NoError(t, err)
	return jf
}

func TestJudge_UnjudgedDocFailsAfterWritingTheFile(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit", "climate report")
	for id := range fx.articles {
		delete(fx.articles, id)
		break
	}

	err := fx.judge(t, "--strategy", "bm25")

	require.Error(t, err)
	jf := fx.annotations(t, "bm25")
	require.NotNil(t, jf.Meta.UnjudgedCount)
	assert.Equal(t, 1, *jf.Meta.UnjudgedCount)
}

func TestJudge_AllowUnjudgedExitsZero(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit", "climate report")
	for id := range fx.articles {
		delete(fx.articles, id)
		break
	}

	assert.NoError(t, fx.judge(t, "--strategy", "bm25", "--allow-unjudged"))
}

func TestJudge_FullyGradedPoolExitsZero(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit", "climate report")

	require.NoError(t, fx.judge(t, "--strategy", "bm25"))

	jf := fx.annotations(t, "bm25")
	require.NotNil(t, jf.Meta.UnjudgedCount)
	assert.Zero(t, *jf.Meta.UnjudgedCount)
}

func TestJudge_QueryWithoutTermsFailsAfterWritingTheFile(t *testing.T) {
	fx := newJudgeFixture(t, "the of a", "climate change summit")
	fx.writePriorBM25(t, "prior-run", judgment.Entry{QueryID: "q-gone"})

	err := fx.judge(t, "--strategy", "bm25", "--allow-unjudged", "--resume")

	var noTerms *judgment.NoQueryTermsError
	require.ErrorAs(t, err, &noTerms)
	assert.Equal(t, "q1", noTerms.QueryID)
	got := gradesByQuery(fx.annotations(t, "bm25"))
	assert.Equal(t, judgment.GradeUnjudged, got["q1"][fx.pool.Queries[0].Docs[0].DocID])
	assert.NotContains(t, got, "q-gone")
}

// A resumed file is this run's artifact and must not keep the prior run's
// provenance.
func TestJudge_ResumedFileCarriesTheNewRunID(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit")
	fx.writePriorBM25(t, "prior-run", judgment.Entry{QueryID: "q1"})

	require.NoError(t, fx.judge(t, "--strategy", "bm25", "--resume"))

	runID := fx.annotations(t, "bm25").Meta.RunID
	assert.NotEmpty(t, runID)
	assert.NotEqual(t, "prior-run", runID)
}

func TestJudge_ManualRerunKeepsHandGrades(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit")
	require.NoError(t, fx.judge(t, "--strategy", "manual"))
	handGraded := fx.annotations(t, "manual")
	handGraded.Queries[0].Docs[0].Grade = judgment.GradeHighly
	require.NoError(t, judgment.WriteFile(handGraded, filepath.Join(fx.track, "trec", "annotations.manual.yaml")))
	newDoc := uuid.New()
	fx.pool.Queries[0].Docs = append(fx.pool.Queries[0].Docs, pool.PooledDoc{DocID: newDoc})
	fx.writePool(t)

	require.NoError(t, fx.judge(t, "--strategy", "manual"))

	got := gradesByQuery(fx.annotations(t, "manual"))["q1"]
	assert.Equal(t, judgment.GradeHighly, got[handGraded.Queries[0].Docs[0].DocID])
	assert.Equal(t, judgment.GradeUnjudged, got[newDoc])
}

func TestJudge_ManualForceDiscardsHandGrades(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit")
	require.NoError(t, fx.judge(t, "--strategy", "manual"))
	handGraded := fx.annotations(t, "manual")
	handGraded.Queries[0].Docs[0].Grade = judgment.GradeHighly
	require.NoError(t, judgment.WriteFile(handGraded, filepath.Join(fx.track, "trec", "annotations.manual.yaml")))

	require.NoError(t, fx.judge(t, "--strategy", "manual", "--force"))

	assert.Equal(t, judgment.GradeUnjudged, fx.annotations(t, "manual").Queries[0].Docs[0].Grade)
}

// A query dropped from the pool since the last run is no longer judged, so its
// leftover placeholders must not fail the resumed run.
func TestJudge_ResumeDropsQueriesThatLeftThePool(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit")
	judge := judgment.NewBM25Strategy().Describe()
	require.NoError(t, judgment.WriteFile(&judgment.File{
		Meta:    meta.Meta{Judge: &judge, PoolRunID: fx.pool.Meta.RunID},
		Queries: []judgment.Entry{{QueryID: "q-gone", Docs: []judgment.GradedDoc{{DocID: uuid.New(), Grade: judgment.GradeUnjudged}}}},
	}, filepath.Join(fx.track, "trec", "annotations.bm25.yaml")))

	require.NoError(t, fx.judge(t, "--strategy", "bm25", "--resume"))

	assert.NotContains(t, gradesByQuery(fx.annotations(t, "bm25")), "q-gone")
}

// Merging into another judge's file would relabel its grades as hand grades.
func TestJudge_ManualRefusesAnotherJudgesFile(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit")
	require.NoError(t, fx.judge(t, "--strategy", "bm25"))
	bm25Path := filepath.Join(fx.track, "trec", "annotations.bm25.yaml")

	err := fx.judge(t, "--strategy", "manual", "--output", bm25Path)

	require.Error(t, err)
	assert.Equal(t, string(judgment.StrategyBM25), fx.annotations(t, "bm25").Judge().Strategy)
}

func TestJudge_ManualForceReplacesAnotherJudgesFile(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit")
	require.NoError(t, fx.judge(t, "--strategy", "bm25"))
	bm25Path := filepath.Join(fx.track, "trec", "annotations.bm25.yaml")

	require.NoError(t, fx.judge(t, "--strategy", "manual", "--output", bm25Path, "--force"))

	assert.Equal(t, string(judgment.StrategyManual), fx.annotations(t, "bm25").Judge().Strategy)
}

// interruptingStore cancels the run when the article interruptAt is fetched.
type interruptingStore struct {
	articleStore
	interruptAt uuid.UUID
	interrupt   context.CancelFunc
}

func (s *interruptingStore) GetByIDs(ctx context.Context, ids []uuid.UUID) ([]document.Article, error) {
	if slices.Contains(ids, s.interruptAt) {
		s.interrupt()
	}
	return s.articleStore.GetByIDs(ctx, ids)
}

// An interrupted run leaves only its intermediate flush, which must carry the
// run's provenance, pool binding and counts like a finished file.
func TestJudge_InterruptedRunLeavesAnIdentifiedFile(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit")
	q2Doc := uuid.New()
	fx.articles[q2Doc] = document.Article{ID: q2Doc, Title: "climate report"}
	fx.pool.Queries = append(fx.pool.Queries, pool.PoolEntry{QueryID: "q2", QueryText: "climate", Docs: []pool.PooledDoc{{DocID: q2Doc}}})
	fx.writePool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &interruptingStore{articleStore: fx.articles, interruptAt: q2Doc, interrupt: cancel}

	cmd := newJudgeCmdWithReader(func(*cobra.Command, string) (storage.Reader, error) { return store, nil })
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{fx.track, "--strategy", "bm25"})
	require.ErrorIs(t, cmd.ExecuteContext(ctx), context.Canceled)

	jf := fx.annotations(t, "bm25")
	assert.NotEmpty(t, jf.Meta.RunID)
	assert.Equal(t, fx.pool.Meta.RunID, jf.Meta.PoolRunID, "--resume reads only this flush, so it must name the pool")
	assert.NotNil(t, jf.Meta.UnjudgedCount)
	assert.Contains(t, gradesByQuery(jf), "q1")
}

func TestJudge_ForceOutsideManualIsRejected(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit")

	err := fx.judge(t, "--strategy", "bm25", "--force")

	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(fx.track, "trec", "annotations.bm25.yaml"))
}

// The judge grades with the package's query vectors, as pool and run rank with
// them: the Ollama here answers with the opposite vector. The pool holds one
// doc stored at the package's vector and ten at the opposite one, so only the
// package's vector puts that doc among the ten nearest.
func TestJudge_VectorGradesWithTheTracksQueryVectors(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewPGContainerWithCleanup(ctx, t)
	track := queryVectorsTrack(t, container.ConnString)
	const queryText = "central bank interest rates"
	queryVector := packageQueryVector(t, track, queryText)
	opposite := make([]float32, len(queryVector))
	for i, v := range queryVector {
		opposite[i] = -v
	}

	conn, err := pgx.Connect(ctx, container.ConnString)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(ctx) })
	fx := &judgeFixture{track: track, articles: articleStore{}}
	entry := pool.PoolEntry{QueryID: "q2", QueryText: queryText}
	store := func(vector []float32) uuid.UUID {
		id := uuid.New()
		_, err := conn.Exec(ctx, `INSERT INTO articles (id, url, title, content, language) VALUES ($1, $2, $3, $4, $5)`,
			id, "https://example.com/"+id.String(), "rates", "content", "eng")
		require.NoError(t, err)
		_, err = conn.Exec(ctx, `INSERT INTO article_embeddings (article_id, model_name, embedding) VALUES ($1, $2, $3)`,
			id, queryVectorsFixtureTag, pgvector.NewVector(vector))
		require.NoError(t, err)
		fx.articles[id] = document.Article{ID: id, Title: "rates"}
		entry.Docs = append(entry.Docs, pool.PooledDoc{DocID: id})
		return id
	}
	docID := store(queryVector)
	for range 10 {
		store(opposite)
	}
	fx.pool = &pool.PoolFile{Meta: meta.New("pool"), Queries: []pool.PoolEntry{entry}}
	fx.writePool(t)

	var ollamaCalls atomic.Int32
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ollamaCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": opposite})
	}))
	t.Cleanup(ollama.Close)
	t.Setenv("EMBEDDING_BASE_URL", ollama.URL)
	t.Setenv("EMBEDDING_MODEL", "qwen3-embedding:0.6b")
	t.Setenv("PG_CONNECTION_STRING", "")

	t.Run("grades with the package vector", func(t *testing.T) {
		require.NoError(t, fx.judge(t, "--strategy", "vector", "--pg", container.ConnString))

		assert.Equal(t, judgment.GradeHighly, gradesByQuery(fx.annotations(t, "vector"))["q2"][docID])
		assert.Zero(t, ollamaCalls.Load())
	})

	t.Run("refuses a package the suite has moved past", func(t *testing.T) {
		suitePath := filepath.Join(track, "suite.yaml")
		raw, err := os.ReadFile(suitePath)
		require.NoError(t, err)
		edited := strings.Replace(string(raw), `"climate change policy"`, `"climate change policies"`, 1)
		require.NoError(t, os.WriteFile(suitePath, []byte(edited), 0o644))

		err = fx.judge(t, "--strategy", "vector", "--pg", container.ConnString)

		var stale *queryvectors.StaleQueryError
		require.ErrorAs(t, err, &stale)
		assert.Equal(t, "q1", stale.QueryID)
	})
}

func packageQueryVector(t *testing.T, track, text string) []float32 {
	t.Helper()
	store, err := queryvectors.Open(context.Background(), queryvectors.Options{
		Package: filepath.Join(track, "trec", "query-vectors"),
		Suite:   filepath.Join(track, "suite.yaml"),
		Model:   queryVectorsFixtureTag,
	})
	require.NoError(t, err)
	vector, err := store.QueryVector(context.Background(), text)
	require.NoError(t, err)
	return vector
}

func TestJudge_KeywordStrategyWritesItsOwnSet(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit")

	require.NoError(t, fx.judge(t, "--strategy", "keyword"))

	assert.Equal(t, "keyword", fx.annotations(t, "keyword").Judge().Strategy)
}

func TestJudge_LexicalIsNoLongerAStrategy(t *testing.T) {
	fx := newJudgeFixture(t, "climate change", "climate change summit")

	err := fx.judge(t, "--strategy", "lexical")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "keyword")
	assert.NoFileExists(t, filepath.Join(fx.track, "trec", "annotations.lexical.yaml"))
}

func TestJudge_PoolQueryWithoutTextFailsNamingIt(t *testing.T) {
	for _, strategy := range []string{"keyword", "bm25"} {
		t.Run(strategy, func(t *testing.T) {
			fx := newJudgeFixture(t, "", "climate change summit")
			fx.pool.Queries[0].QueryDesc = "Coverage of climate change"
			fx.writePool(t)

			err := fx.judge(t, "--strategy", strategy)

			require.Error(t, err)
			assert.Contains(t, err.Error(), `"q1"`)
			assert.NoFileExists(t, filepath.Join(fx.track, "trec", "annotations."+strategy+".yaml"))
		})
	}
}

func TestJudge_ManualNeedsNoQueryText(t *testing.T) {
	fx := newJudgeFixture(t, "", "climate change summit")

	require.NoError(t, fx.judge(t, "--strategy", "manual"))
}

// keyword, bm25, vector and hybrid grade the query's text through new rules,
// so a file their v1 rubric graded cannot be resumed.
func TestCheckResumeCompat_RefusesTheDeterministicJudgesV1Rubric(t *testing.T) {
	for _, strategy := range []judgment.Strategy{
		judgment.NewKeywordStrategy(),
		judgment.NewBM25Strategy(),
		judgment.NewVectorStrategyWithStore(stubVectorStore{}, "qwen3-embedding:0.6b@1024"),
		judgment.NewHybridStrategyWithStore(stubVectorStore{}, "qwen3-embedding:0.6b@1024"),
	} {
		t.Run(strategy.Name(), func(t *testing.T) {
			v1 := strategy.Describe()
			v1.RubricVersion = "v1"

			err := checkResumeCompat(fileWith(v1), strategy)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "rubric v1")
		})
	}
}

func TestJudge_UnparsableBooleanTextFailsAfterWritingTheFile(t *testing.T) {
	fx := newJudgeFixture(t, "climate AND (politics", "climate change summit")
	fx.pool.Queries[0].Category = "boolean"
	fx.writePool(t)

	out, err := fx.judgeOutput(t, "--strategy", "bm25", "--allow-unjudged")

	var unparsed *judgment.BooleanTextError
	require.ErrorAs(t, err, &unparsed)
	assert.Equal(t, "q1", unparsed.QueryID)
	jf := fx.annotations(t, "bm25")
	assert.Equal(t, judgment.GradeUnjudged, gradesByQuery(jf)["q1"][fx.pool.Queries[0].Docs[0].DocID])
	require.NotNil(t, jf.Meta.GradedCount)
	require.NotNil(t, jf.Meta.UnjudgedCount)
	assert.Equal(t, 0, *jf.Meta.GradedCount)
	assert.Equal(t, 1, *jf.Meta.UnjudgedCount)
	// Every flush already writes the same file, so only the closing line shows
	// the command finalised it before failing.
	assert.Regexp(t, `Judgments written: \S+/trec/annotations\.bm25\.yaml`, out)
}
