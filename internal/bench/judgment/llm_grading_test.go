package judgment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/pool"
	"github.com/tadjov/tusker/internal/bench/suite"
	"github.com/tadjov/tusker/internal/types/document"
)

// The model grades the query a user would type, exactly as stored; the suite
// author's description of it never reaches the model.
func TestLLMJudge_PromptCarriesTheQueryTextAndNeverItsDescription(t *testing.T) {
	q := GradingQuery{ID: "q1", Text: `"climate change" summit`, Description: "Coverage of the yearly UN climate conference"}
	docs := []GradingDoc{
		{ID: uuid.New(), Title: "Climate change summit opens"},
		{ID: uuid.New(), Title: "Summit delegates arrive"},
	}
	provider := &fakeLLMProvider{grade: func(uuid.UUID) int { return 2 }}
	s := &LLMStrategy{provider: provider}

	_, err := s.Grade(context.Background(), q, docs[0])
	require.NoError(t, err)
	_, err = s.GradeBatch(context.Background(), q, docs)
	require.NoError(t, err)

	require.Len(t, provider.requests(), 2)
	for _, req := range provider.requests() {
		assert.Contains(t, req.User, "Query: "+q.Text)
		assert.NotContains(t, req.User, q.Description)
		assert.NotContains(t, req.System, q.Description)
	}
}

// Phrase, boolean and filter constraints are decided in code: a doc that
// breaks one grades 0 without a request, whatever the model would say, and
// the model grades only the docs that keep it.
func TestLLMJudge_GradesADocBreakingTheQuerysConstraintZeroWithoutAsking(t *testing.T) {
	tests := []struct {
		name     string
		entry    pool.PoolEntry
		admitted document.Article
		broken   document.Article
	}{
		{
			name:     "phrase",
			entry:    pool.PoolEntry{QueryText: "solar power", Category: suite.CategoryPhrase},
			admitted: llmArticle("Solar power plant opens", "reuters.com"),
			broken:   llmArticle("Power prices squeeze solar firms", "reuters.com"),
		},
		{
			name:     "not",
			entry:    pool.PoolEntry{QueryText: "climate AND NOT politics", Category: suite.CategoryBoolean},
			admitted: llmArticle("Climate scientists publish ice data", "reuters.com"),
			broken:   llmArticle("Climate politics heats up in parliament", "reuters.com"),
		},
		{
			name:     "declared filter",
			entry:    pool.PoolEntry{QueryText: "climate", Category: suite.CategoryKeyword, Filters: suite.Filters{PublisherDomain: "reuters.com"}},
			admitted: llmArticle("Climate summit opens", "reuters.com"),
			broken:   llmArticle("Climate summit opens", "bbc.co.uk"),
		},
	}
	for _, tt := range tests {
		for _, batch := range []int{0, 2} {
			t.Run(fmt.Sprintf("%s/batch %d", tt.name, batch), func(t *testing.T) {
				entry := tt.entry
				entry.QueryID = "q1"
				entry.Docs = []pool.PooledDoc{{DocID: tt.broken.ID}, {DocID: tt.admitted.ID}}
				provider := &fakeLLMProvider{grade: func(uuid.UUID) int { return GradeHighly }}

				jf, err := llmRunner(provider, []document.Article{tt.admitted, tt.broken}, RunnerConfig{BatchSize: batch}).
					Run(context.Background(), &pool.PoolFile{Queries: []pool.PoolEntry{entry}})

				require.NoError(t, err)
				grades := gradesOf(jf)
				assert.Equal(t, GradeNotRelev, grades[tt.broken.ID])
				assert.Equal(t, GradeHighly, grades[tt.admitted.ID])
				for _, req := range provider.requests() {
					assert.NotContains(t, req.User, tt.broken.ID.String(), "no request for a doc the constraint refuses")
				}
			})
		}
	}
}

// A boolean text that does not parse leaves its constraint undecided, so the
// query stays unjudged rather than graded by the model alone.
func TestLLMJudge_LeavesAQueryWithAnUnparsableBooleanUnjudged(t *testing.T) {
	for _, batch := range []int{0, 2} {
		t.Run(fmt.Sprintf("batch %d", batch), func(t *testing.T) {
			doc := llmArticle("Climate politics heats up", "reuters.com")
			entry := pool.PoolEntry{QueryID: "q1", QueryText: "climate AND (politics", Category: suite.CategoryBoolean, Docs: []pool.PooledDoc{{DocID: doc.ID}}}
			provider := &fakeLLMProvider{grade: func(uuid.UUID) int { return GradeHighly }}

			jf, err := llmRunner(provider, []document.Article{doc}, RunnerConfig{BatchSize: batch}).
				Run(context.Background(), &pool.PoolFile{Queries: []pool.PoolEntry{entry}})

			var unparsed *BooleanTextError
			require.ErrorAs(t, err, &unparsed)
			assert.Equal(t, GradeUnjudged, gradesOf(jf)[doc.ID])
			assert.Empty(t, provider.requests())
		})
	}
}

// A doc is shown the same way whether it is graded alone or in a batch: the
// same rubric, its title and description in full, and its content up to the
// same cut, which sends a median cc-news article (1,744 characters) whole.
func TestLLMJudge_GradesOneDocAndABatchUnderTheSameRubricAndContentCut(t *testing.T) {
	const medianArticleChars = 1744
	median := GradingDoc{
		ID:          uuid.New(),
		Title:       "Sava floods reach Belgrade",
		Description: strings.Repeat("Rijeka Sava se izlila. ", 40),
		Content:     strings.Repeat("Ž", medianArticleChars),
	}
	long := GradingDoc{ID: uuid.New(), Title: "Danube floods", Content: strings.Repeat("Đ", 3*medianArticleChars)}
	q := GradingQuery{ID: "q1", Text: "floods"}
	provider := &fakeLLMProvider{grade: func(uuid.UUID) int { return GradeRelevant }}
	s := &LLMStrategy{provider: provider}

	for _, doc := range []GradingDoc{median, long} {
		_, err := s.Grade(context.Background(), q, doc)
		require.NoError(t, err)
	}
	_, err := s.GradeBatch(context.Background(), q, []GradingDoc{median, long})
	require.NoError(t, err)

	reqs := provider.requests()
	require.Len(t, reqs, 3)
	alone, batched := reqs[:2], reqs[2]
	for i, doc := range []GradingDoc{median, long} {
		assert.Equal(t, batched.System, alone[i].System, "one rubric")
		assert.Equal(t, contentShown(t, batched.User, doc), contentShown(t, alone[i].User, doc), "one content cut")
	}
	assert.Contains(t, batched.User, median.Title)
	assert.Contains(t, batched.User, median.Description)
	assert.Equal(t, median.Content, contentShown(t, batched.User, median))
	assert.NotContains(t, batched.User, long.Content)
}

// contentShown is the content line a prompt shows for doc, the doc's block
// starting at its id.
func contentShown(t *testing.T, prompt string, doc GradingDoc) string {
	t.Helper()
	_, block, found := strings.Cut(prompt, doc.ID.String())
	require.True(t, found, "prompt names doc %s", doc.ID)
	_, content, found := strings.Cut(block, "Content: ")
	require.True(t, found, "prompt shows doc %s's content", doc.ID)
	content, _, _ = strings.Cut(content, "\n")
	return strings.TrimSuffix(content, "...")
}

// By default each request holds one query and one doc, so its bytes are the
// same wherever the pool lists the doc and whichever engine found it.
func TestLLMJudge_RequestForADocIsTheSameWhereverThePoolListsIt(t *testing.T) {
	articles := llmArticles("Floods reach Belgrade", "Sava rises", "Danube rises")
	target := articles[0].ID
	first := pool.PoolEntry{QueryID: "q1", QueryText: "floods", Docs: []pool.PooledDoc{
		{DocID: articles[0].ID, Sources: []string{"pg"}},
		{DocID: articles[1].ID, Sources: []string{"pg"}},
		{DocID: articles[2].ID, Sources: []string{"es"}},
	}}
	last := pool.PoolEntry{QueryID: "q1", QueryText: "floods", Docs: []pool.PooledDoc{
		{DocID: articles[2].ID, Sources: []string{"pg"}},
		{DocID: articles[1].ID, Sources: []string{"es"}},
		{DocID: articles[0].ID, Sources: []string{"es", "pg"}},
	}}

	var requests []CompletionRequest
	for _, entry := range []pool.PoolEntry{first, last} {
		provider := &fakeLLMProvider{grade: func(uuid.UUID) int { return GradeRelevant }}
		_, err := llmRunner(provider, articles, RunnerConfig{}).Run(context.Background(), &pool.PoolFile{Queries: []pool.PoolEntry{entry}})
		require.NoError(t, err)
		for _, req := range provider.requests() {
			if strings.Contains(req.User, target.String()) {
				requests = append(requests, req)
			}
		}
	}

	require.Len(t, requests, 2)
	assert.Equal(t, []uuid.UUID{target}, docIDsIn(requests[0].User), "the request holds that doc alone")
	assert.Equal(t, requests[0], requests[1])
}

// --batch groups docs by a seeded shuffle of the query's pool: the same pool
// and seed give the same batches in any pool order, and another seed others.
func TestLLMJudge_BatchesComeFromASeededShuffleOfThePool(t *testing.T) {
	articles := floodReports(20)
	byEngine := pool.PoolEntry{QueryID: "q1", QueryText: "flood"}
	for i, a := range articles {
		engine := "pg"
		if i >= len(articles)/2 {
			engine = "es"
		}
		byEngine.Docs = append(byEngine.Docs, pool.PooledDoc{DocID: a.ID, Sources: []string{engine}})
	}
	reordered := byEngine
	reordered.Docs = append(slices.Clone(byEngine.Docs[3:]), byEngine.Docs[:3]...)

	seeded := llmBatches(t, articles, &pool.PoolFile{Queries: []pool.PoolEntry{byEngine}}, RunnerConfig{BatchSize: 5, BatchSeed: 1})
	require.Len(t, seeded, 4)
	assert.Equal(t, seeded, llmBatches(t, articles, &pool.PoolFile{Queries: []pool.PoolEntry{byEngine}}, RunnerConfig{BatchSize: 5, BatchSeed: 1}), "same pool and seed")
	assert.Equal(t, seeded, llmBatches(t, articles, &pool.PoolFile{Queries: []pool.PoolEntry{reordered}}, RunnerConfig{BatchSize: 5, BatchSeed: 1}), "pool order and engines play no part")
	assert.NotEqual(t, seeded, llmBatches(t, articles, &pool.PoolFile{Queries: []pool.PoolEntry{byEngine}}, RunnerConfig{BatchSize: 5, BatchSeed: 2}), "another seed")
}

// A resumed run, or one that cannot read every pooled article, grades each
// remaining doc in what is left of the batch a fresh run puts it in, so it
// never pairs docs a fresh run would not.
func TestLLMJudge_ResumedBatchesAreWhatIsLeftOfTheFreshOnes(t *testing.T) {
	articles := floodReports(20)
	entry := pool.PoolEntry{QueryID: "q1", QueryText: "flood"}
	for _, a := range articles {
		entry.Docs = append(entry.Docs, pool.PooledDoc{DocID: a.ID})
	}
	pf := &pool.PoolFile{Queries: []pool.PoolEntry{entry}}
	cfg := RunnerConfig{BatchSize: 5, BatchSeed: 1}
	fresh := llmBatches(t, articles, pf, cfg)

	prior := Entry{QueryID: "q1"}
	for _, a := range articles[:7] {
		prior.Docs = append(prior.Docs, GradedDoc{DocID: a.ID, Grade: GradeRelevant})
	}
	cfg.Existing = &File{Queries: []Entry{prior}}
	readable := slices.Delete(slices.Clone(articles), 12, 13)
	resumed := llmBatches(t, readable, pf, cfg)

	require.NotEmpty(t, resumed)
	assert.True(t, slices.ContainsFunc(resumed, func(batch []uuid.UUID) bool { return len(batch) == 1 }),
		"the resume leaves a one-doc batch, still sent as a batch")
	for _, batch := range resumed {
		assert.True(t, slices.ContainsFunc(fresh, func(whole []uuid.UUID) bool {
			return !slices.ContainsFunc(batch, func(id uuid.UUID) bool { return !slices.Contains(whole, id) })
		}), "resumed batch %v is part of no fresh batch", batch)
	}
}

// The judge block names the batch size and seed a batched run graded with,
// and neither for one doc per request.
func TestRunnerConfig_JudgeRecordsBatching(t *testing.T) {
	llm := &LLMStrategy{provider: &fakeLLMProvider{}}

	perDoc := RunnerConfig{Strategy: llm, BatchSeed: 7}.Judge()
	batched := RunnerConfig{Strategy: llm, BatchSize: 5, BatchSeed: 7}.Judge()

	assert.Nil(t, perDoc.Batching)
	assert.Equal(t, 5, batched.Batching.Size)
	assert.Equal(t, uint64(7), batched.Batching.Seed)
	assert.Equal(t, llm.Describe().Model, batched.Model)
}

// An answer without a grade, or with a null one, leaves the doc unjudged for
// a retry, never graded 0.
func TestLLMJudge_LeavesADocWithoutAGradeUnjudged(t *testing.T) {
	tests := []struct {
		name  string
		batch int
		reply func(missing, graded uuid.UUID, req CompletionRequest) string
	}{
		{name: "relevance instead of grade", batch: 2, reply: func(missing, graded uuid.UUID, req CompletionRequest) string {
			if isBatchRequest(req) {
				return `[{"doc_id":"` + missing.String() + `","relevance":3},{"doc_id":"` + graded.String() + `","grade":2}]`
			}
			return `{"doc_id":"` + missing.String() + `","relevance":3}`
		}},
		{name: "null grade, one doc per request", reply: func(missing, graded uuid.UUID, req CompletionRequest) string {
			if strings.Contains(req.User, missing.String()) {
				return `{"doc_id":"` + missing.String() + `","grade":null}`
			}
			return `{"doc_id":"` + graded.String() + `","grade":2}`
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			articles := llmArticles("Floods reach Belgrade", "Sava rises")
			missing, graded := articles[0].ID, articles[1].ID
			entry := pool.PoolEntry{QueryID: "q1", QueryText: "floods", Docs: []pool.PooledDoc{{DocID: missing}, {DocID: graded}}}
			provider := &fakeLLMProvider{reply: func(req CompletionRequest) string { return tt.reply(missing, graded, req) }}

			jf, err := llmRunner(provider, articles, RunnerConfig{BatchSize: tt.batch}).
				Run(context.Background(), &pool.PoolFile{Queries: []pool.PoolEntry{entry}})

			require.NoError(t, err)
			assert.Equal(t, GradeUnjudged, gradesOf(jf)[missing])
			assert.Equal(t, GradeRelevant, gradesOf(jf)[graded])
		})
	}
}

// A grade that fails is no grade: Grade reports it as unjudged, never as 0.
func TestLLMJudge_GradeReportsAFailedGradeAsUnjudged(t *testing.T) {
	tests := map[string]*fakeLLMProvider{
		"the request fails":      {err: errors.New("connection refused")},
		"the reply has no grade": {reply: func(CompletionRequest) string { return "I cannot grade this." }},
	}
	for name, provider := range tests {
		t.Run(name, func(t *testing.T) {
			grade, err := (&LLMStrategy{provider: provider}).Grade(context.Background(),
				GradingQuery{ID: "q1", Text: "floods"}, GradingDoc{ID: uuid.New(), Title: "Floods reach Belgrade"})

			require.Error(t, err)
			assert.Equal(t, GradeUnjudged, grade)
		})
	}
}

// fakeLLMProvider grades every doc a request names with grade(doc), answering
// a batch request with an array, however few docs it holds, and a single-doc
// request with an object. A set err fails every request.
type fakeLLMProvider struct {
	grade func(uuid.UUID) int
	reply func(CompletionRequest) string
	err   error

	mu   sync.Mutex
	seen []CompletionRequest
}

func (f *fakeLLMProvider) Name() Provider     { return "fake" }
func (f *fakeLLMProvider) Model() string      { return "fake-model" }
func (f *fakeLLMProvider) Effort() Effort     { return EffortLow }
func (f *fakeLLMProvider) Invocation() string { return "fake" }

func (f *fakeLLMProvider) Complete(_ context.Context, req CompletionRequest) (string, error) {
	f.mu.Lock()
	f.seen = append(f.seen, req)
	f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	if f.reply != nil {
		return f.reply(req), nil
	}
	var entries []gradeEntry
	for _, id := range docIDsIn(req.User) {
		entries = append(entries, gradeEntry{DocID: id.String(), Grade: f.grade(id)})
	}
	var out []byte
	var err error
	if isBatchRequest(req) {
		out, err = json.Marshal(entries)
	} else {
		out, err = json.Marshal(entries[0])
	}
	return string(out), err
}

// isBatchRequest reports whether GradeBatch built req: only it asks for the
// batch budget.
func isBatchRequest(req CompletionRequest) bool { return req.MaxTokens == llmBatchMaxOutputTokens }

func (f *fakeLLMProvider) requests() []CompletionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CompletionRequest(nil), f.seen...)
}

type gradeEntry struct {
	DocID string `json:"doc_id"`
	Grade int    `json:"grade"`
}

// docIDsIn returns the doc ids a prompt names, in the order it names them.
func docIDsIn(prompt string) []uuid.UUID {
	var ids []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, word := range strings.FieldsFunc(prompt, func(r rune) bool {
		return r != '-' && (r < '0' || r > '9') && (r < 'a' || r > 'f')
	}) {
		if id, err := uuid.Parse(word); err == nil && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

func llmArticles(titles ...string) []document.Article {
	out := make([]document.Article, len(titles))
	for i, title := range titles {
		out[i] = document.Article{ID: uuid.New(), Title: title}
	}
	return out
}

func llmArticle(title, publisherDomain string) document.Article {
	return document.Article{ID: uuid.New(), Title: title, PublisherDomain: publisherDomain}
}

// llmRunner grades articles through an LLM judge on provider, batched as cfg
// says.
func llmRunner(provider LLMProvider, articles []document.Article, cfg RunnerConfig) *Runner {
	store := map[uuid.UUID]document.Article{}
	for _, a := range articles {
		store[a.ID] = a
	}
	cfg.Strategy = &LLMStrategy{provider: provider}
	cfg.Reader = &stubStorageReader{articles: store}
	return NewRunner(cfg)
}

func gradesOf(jf *File) map[uuid.UUID]int {
	out := map[uuid.UUID]int{}
	for _, qe := range jf.Queries {
		for _, d := range qe.Docs {
			out[d.DocID] = d.Grade
		}
	}
	return out
}

// floodReports are n articles with fixed ids, so a seeded shuffle of them
// comes out the same on every run.
func floodReports(n int) []document.Article {
	out := make([]document.Article, n)
	for i := range out {
		title := fmt.Sprintf("Flood report %d", i)
		out[i] = document.Article{ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(title)), Title: title}
	}
	return out
}

// llmBatches grades pf through an LLM judge and returns the doc ids of each
// request, each sorted, the requests ordered by their first id.
func llmBatches(t *testing.T, articles []document.Article, pf *pool.PoolFile, cfg RunnerConfig) [][]uuid.UUID {
	t.Helper()
	provider := &fakeLLMProvider{grade: func(uuid.UUID) int { return GradeRelevant }}
	_, err := llmRunner(provider, articles, cfg).Run(context.Background(), pf)
	require.NoError(t, err)
	var out [][]uuid.UUID
	for _, req := range provider.requests() {
		require.Equal(t, llmBatchMaxOutputTokens, req.MaxTokens, "every request is a batch, none a per-doc fallback")
		ids := docIDsIn(req.User)
		slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		out = append(out, ids)
	}
	slices.SortFunc(out, func(a, b []uuid.UUID) int { return strings.Compare(a[0].String(), b[0].String()) })
	return out
}
