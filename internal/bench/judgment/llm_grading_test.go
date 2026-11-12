package judgment

import (
	"context"
	"encoding/json"
	"fmt"
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

				jf, err := llmRunner(provider, []document.Article{tt.admitted, tt.broken}, batch).
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

			jf, err := llmRunner(provider, []document.Article{doc}, batch).
				Run(context.Background(), &pool.PoolFile{Queries: []pool.PoolEntry{entry}})

			var unparsed *BooleanTextError
			require.ErrorAs(t, err, &unparsed)
			assert.Equal(t, GradeUnjudged, gradesOf(jf)[doc.ID])
			assert.Empty(t, provider.requests())
		})
	}
}

// fakeLLMProvider grades every doc a request names with grade(doc), answering
// a batch request with an array and a single-doc request with an object.
type fakeLLMProvider struct {
	grade func(uuid.UUID) int
	reply func(CompletionRequest) string

	mu   sync.Mutex
	seen []CompletionRequest
}

func (f *fakeLLMProvider) Name() Provider          { return "fake" }
func (f *fakeLLMProvider) Model() string           { return "fake-model" }
func (f *fakeLLMProvider) PreferredBatchSize() int { return 1 }
func (f *fakeLLMProvider) Invocation() string      { return "fake" }

func (f *fakeLLMProvider) Complete(_ context.Context, req CompletionRequest) (string, error) {
	f.mu.Lock()
	f.seen = append(f.seen, req)
	f.mu.Unlock()
	if f.reply != nil {
		return f.reply(req), nil
	}
	var entries []gradeEntry
	for _, id := range docIDsIn(req.User) {
		entries = append(entries, gradeEntry{DocID: id.String(), Grade: f.grade(id)})
	}
	var out []byte
	var err error
	if strings.Contains(req.User, "JSON array") {
		out, err = json.Marshal(entries)
	} else {
		out, err = json.Marshal(entries[0])
	}
	return string(out), err
}

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

func llmArticle(title, publisherDomain string) document.Article {
	return document.Article{ID: uuid.New(), Title: title, PublisherDomain: publisherDomain}
}

func llmRunner(provider LLMProvider, articles []document.Article, batchSize int) *Runner {
	store := map[uuid.UUID]document.Article{}
	for _, a := range articles {
		store[a.ID] = a
	}
	return NewRunner(RunnerConfig{
		Strategy:  &LLMStrategy{provider: provider},
		Reader:    &stubStorageReader{articles: store},
		BatchSize: batchSize,
	})
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
