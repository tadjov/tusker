package judgment

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/bench/meta"
)

const (
	llmMaxOutputTokens      = 64
	llmBatchMaxOutputTokens = 4096
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
	}
}

func (s *LLMStrategy) Grade(ctx context.Context, q GradingQuery, doc GradingDoc) (int, error) {
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

func (s *LLMStrategy) PreferredBatchSize() int { return s.provider.PreferredBatchSize() }

// GradeBatch implements BatchStrategy. One provider call per batch; the caller
// (judgment.Runner) chunks and handles docs that come back missing or malformed.
func (s *LLMStrategy) GradeBatch(ctx context.Context, q GradingQuery, docs []GradingDoc) ([]GradedDoc, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	text, err := s.provider.Complete(ctx, CompletionRequest{
		System:    BatchSystemPrompt,
		User:      BuildBatchGradingPrompt(q, docs),
		MaxTokens: llmBatchMaxOutputTokens,
	})
	if err != nil {
		return nil, err
	}
	parsed, missing, err := ParseBatchGradeJSON(text, docs)
	if err != nil {
		return nil, fmt.Errorf("batch parse: %w", err)
	}
	if len(missing) > 0 {
		// Surface partial-success: the Runner retries the missing IDs
		// individually via Grade(). Returning nil error here would discard the
		// parsed entries, so wrap as a typed sentinel.
		return parsed, &PartialBatchError{Missing: missing, Got: len(parsed), Want: len(docs)}
	}
	return parsed, nil
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
