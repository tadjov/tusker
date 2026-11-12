package judgment

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
