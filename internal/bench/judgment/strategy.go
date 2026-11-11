package judgment

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/bench/meta"
	"github.com/tadjov/tusker/internal/storage"
)

// GradingQuery is the query a judge grades against.
type GradingQuery struct {
	ID string
	// Text is the query as a user would type it. The deterministic judges grade
	// it and look its vector up by it; Description only describes it.
	Text        string
	Description string
	Category    string
}

// GradingDoc is a single article candidate to grade.
type GradingDoc struct {
	ID          uuid.UUID
	Title       string
	Description string
	Content     string
}

// Strategy names a grader. It grades through one of DocStrategy,
// BatchStrategy or PoolStrategy.
//
// Describe is mandatory: every artifact must attest who graded it, so a
// strategy that cannot describe itself has no business writing qrels.
type Strategy interface {
	Name() string
	Describe() meta.Judge
}

// DocStrategy grades one (query, doc) pair at a time. Implementations must be
// safe for concurrent calls — the runner dispatches multiple goroutines.
type DocStrategy interface {
	Strategy
	Grade(ctx context.Context, q GradingQuery, doc GradingDoc) (int, error)
}

// BatchStrategy is an optional capability: strategies that can grade N docs
// in a single LLM call should implement it. The runner detects it via type
// assertion and prefers GradeBatch over Grade when present.
//
// The pattern follows Anthropic's "LLM as judge — batched" cookbook:
//   - one system prompt sets the rubric
//   - one user message containing the query + numbered candidates
//   - response is a single JSON array, one entry per candidate
//
// Implementations MUST tolerate partial responses: if the model returns N-k
// entries, return what was parsed and let the runner re-dispatch the missing
// docs through Grade() as a fallback.
type BatchStrategy interface {
	DocStrategy
	PreferredBatchSize() int
	GradeBatch(ctx context.Context, q GradingQuery, docs []GradingDoc) ([]GradedDoc, error)
}

// PoolStrategy is an optional capability for strategies whose grades are
// relative to the rest of the pool. The runner hands GradePool every doc in the
// query's pool in one call, including docs a prior run already graded, so a
// grade does not depend on batching or resume.
type PoolStrategy interface {
	Strategy
	GradePool(ctx context.Context, q GradingQuery, docs []GradingDoc) ([]GradedDoc, error)
}

type StrategyKind string

// Strategy taxonomy. Heuristic strategies derive grades from algorithmic
// signals; LLM strategies call out to a model; manual is a placeholder for
// human grading.
const (
	// StrategyKeyword Heuristic.
	StrategyKeyword StrategyKind = "keyword" // token-overlap baseline
	StrategyBM25    StrategyKind = "bm25"    // pool-local Okapi BM25
	StrategyVector  StrategyKind = "vector"  // embedding cosine similarity
	StrategyHybrid  StrategyKind = "hybrid"  // BM25 + vector fusion

	// StrategyLLM LLM-as-judge. One strategy for every vendor and transport —
	// which endpoint it talks to is a Provider (claude-cli, claude-api, later
	// codex-*, chatgpt-*), selected with --provider.
	StrategyLLM StrategyKind = "llm"

	// StrategyManual Human.
	StrategyManual StrategyKind = "manual"
)

type StrategyOptions struct {
	APIKey string
	// Provider selects the endpoint behind StrategyLLM. Empty means
	// DefaultProvider.
	Provider Provider
	// Model is the judge model — a full id or a provider alias such as "haiku".
	// Empty means the provider's default. The provider expands it, so the judge
	// block always records a concrete id.
	Model       string
	APIBaseURL  string
	CLIBinary   string
	Concurrency int

	// VectorStore backs the vector / hybrid strategies: document vectors are
	// read from it and the query is embedded through it. Engine-agnostic
	// (Postgres today, ES later); built by the caller via storage/factory.
	VectorStore storage.VectorStore
	// EmbeddingModel labels the judge block's model for vector/hybrid.
	EmbeddingModel string
}

func NewStrategy(kind StrategyKind, opts StrategyOptions) (Strategy, error) {
	switch kind {
	case StrategyKeyword:
		return NewKeywordStrategy(), nil
	case StrategyBM25:
		return NewBM25Strategy(), nil
	case StrategyVector:
		return NewVectorStrategy(opts)
	case StrategyHybrid:
		return NewHybridStrategy(opts)
	case StrategyLLM:
		return NewLLMStrategy(opts)
	case StrategyManual:
		return NewManualStrategy(), nil
	default:
		return nil, fmt.Errorf("unknown strategy %q (known: %s)", kind, strings.Join(KnownStrategies(), ", "))
	}
}

// NewStrategyFromSelector resolves a judgment-set name ("keyword",
// "claude-cli", …) and constructs the strategy that produced it. Used on read
// paths; bench judge builds strategies from explicit --strategy/--provider.
func NewStrategyFromSelector(sel string, opts StrategyOptions) (Strategy, error) {
	kind, provider, err := ParseSelector(sel)
	if err != nil {
		return nil, err
	}
	opts.Provider = provider
	return NewStrategy(kind, opts)
}
