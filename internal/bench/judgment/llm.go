package judgment

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/bench/meta"
)

// Output budgets leave room for adaptive thinking before the JSON grades.
const (
	llmMaxOutputTokens      = 2048
	llmBatchMaxOutputTokens = 8192
)

// LLMStrategy is the LLM-as-judge strategy. It owns the rubric, batching and
// response parsing; reaching a vendor is delegated to an LLMProvider, so the
// same grading behaviour runs over a local CLI subprocess or a vendor HTTP API
// and stays comparable across both.
type LLMStrategy struct {
	provider LLMProvider
}

func NewLLMStrategy(opts StrategyOptions) (*LLMStrategy, error) {
	p, err := newProvider(opts.Provider, opts)
	if err != nil {
		return nil, err
	}
	return &LLMStrategy{provider: p}, nil
}

func (s *LLMStrategy) Name() string { return string(StrategyLLM) }

// Describe reports the grader for the artifact's judge block. The model is the
// provider's resolved id — aliases are expanded at construction — so the qrels
// name a concrete model and the resume guard can catch a grader swap.
func (s *LLMStrategy) Describe() meta.Judge {
	return meta.Judge{
		Strategy:      string(StrategyLLM),
		Provider:      string(s.provider.Name()),
		Model:         s.provider.Model(),
		PromptVersion: PromptVersion,
		Effort:        string(s.provider.Effort()),
		Invocation:    s.provider.Invocation(),
	}
}

// Grade gives 0 without a request to a doc the query's filters or category
// gate refuse; the model grades only how well the rest answer the query.
func (s *LLMStrategy) Grade(ctx context.Context, q GradingQuery, doc GradingDoc) (int, error) {
	tq, err := readTermQuery(q)
	if err != nil {
		return GradeUnjudged, err
	}
	if !tq.admits(analyzeDoc(doc)) {
		return GradeNotRelev, nil
	}
	text, err := s.provider.Complete(ctx, CompletionRequest{
		System:    systemPrompt,
		User:      BuildGradingPrompt(q, doc),
		MaxTokens: llmMaxOutputTokens,
	})
	if err != nil {
		return 0, err
	}
	grade, err := ParseGradeJSON(text, doc.ID.String())
	if err != nil {
		return 0, fmt.Errorf("doc %s: %w", doc.ID, err)
	}
	return grade, nil
}

// PreferredBatchSize is one doc per request, so a doc's grade never depends on
// the docs it would share a request with.
func (s *LLMStrategy) PreferredBatchSize() int { return 1 }

// GradeBatch implements BatchStrategy, gating docs as Grade does: one provider
// call per batch for the docs the gate admits, none when it admits none.
func (s *LLMStrategy) GradeBatch(ctx context.Context, q GradingQuery, docs []GradingDoc) ([]GradedDoc, error) {
	tq, err := readTermQuery(q)
	if err != nil {
		return nil, err
	}
	var refused []GradedDoc
	var admitted []GradingDoc
	for _, d := range docs {
		if tq.admits(analyzeDoc(d)) {
			admitted = append(admitted, d)
		} else {
			refused = append(refused, GradedDoc{DocID: d.ID, Grade: GradeNotRelev})
		}
	}
	if len(admitted) == 0 {
		return refused, nil
	}
	text, err := s.provider.Complete(ctx, CompletionRequest{
		System:    systemPrompt,
		User:      BuildBatchGradingPrompt(q, admitted),
		MaxTokens: llmBatchMaxOutputTokens,
	})
	if err != nil {
		return nil, err
	}
	parsed, missing, err := ParseBatchGradeJSON(text, admitted)
	if err != nil {
		return nil, fmt.Errorf("batch parse: %w", err)
	}
	graded := append(refused, parsed...)
	if len(missing) > 0 {
		// Surface partial-success: the Runner retries the missing IDs
		// individually via Grade(). Returning nil error here would discard the
		// parsed entries, so wrap as a typed sentinel.
		return graded, &PartialBatchError{Missing: missing, Got: len(graded), Want: len(docs)}
	}
	return graded, nil
}

// PartialBatchError lets the runner distinguish "the LLM returned only part of
// the batch — retry the missing IDs" from a hard transport/parse failure.
type PartialBatchError struct {
	Missing []uuid.UUID
	Got     int
	Want    int
}

func (e *PartialBatchError) Error() string {
	return fmt.Sprintf("batch partial: got %d/%d, %d missing", e.Got, e.Want, len(e.Missing))
}
