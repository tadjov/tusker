package judgment

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Provider names one LLM endpoint behind StrategyLLM: a vendor plus the
// transport used to reach it. The strategy owns prompting, batching and
// parsing; a provider owns only "send this prompt, return the text". Adding
// Codex or ChatGPT means registering a provider — no strategy code changes.
type Provider string

const (
	ProviderClaudeCLI Provider = "claude-cli"
	ProviderClaudeAPI Provider = "claude-api"
)

// CompletionRequest is one grading call: a system prompt setting the rubric and
// a user payload carrying the query plus candidates.
type CompletionRequest struct {
	System    string
	User      string
	MaxTokens int
}

// LLMProvider executes grading prompts against one vendor endpoint.
type LLMProvider interface {
	Name() Provider
	// Model is the resolved model id this provider will use — an alias such as
	// "haiku" is expanded at construction, so this always names a concrete
	// model for the artifact's judge block.
	Model() string
	// PreferredBatchSize is how many candidates this endpoint grades well in one
	// call. Subprocess transports favour smaller batches than HTTP ones.
	PreferredBatchSize() int
	// Complete sends one prompt and returns the assistant's raw text.
	Complete(ctx context.Context, req CompletionRequest) (string, error)
}

// providers is the registry StrategyLLM resolves against. Register a vendor
// here and its name becomes valid everywhere: --provider, --judgments,
// spec.defaults.judgments, and the annotations filename.
var providers = map[Provider]func(StrategyOptions) (LLMProvider, error){
	ProviderClaudeCLI: newClaudeCLIProvider,
	ProviderClaudeAPI: newClaudeAPIProvider,
}

// DefaultProvider is used when --provider is omitted. The CLI transport bills
// through an existing Claude subscription rather than per-token API credit, so
// it is the cheap default.
const DefaultProvider = ProviderClaudeCLI

func newProvider(name Provider, opts StrategyOptions) (LLMProvider, error) {
	if name == "" {
		name = DefaultProvider
	}
	factory, ok := providers[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (known: %s)", name, strings.Join(KnownProviders(), ", "))
	}
	return factory(opts)
}

// KnownProviders returns every registered provider name, sorted.
func KnownProviders() []string {
	out := make([]string, 0, len(providers))
	for name := range providers {
		out = append(out, string(name))
	}
	sort.Strings(out)
	return out
}

// JudgmentSetName is the on-disk name of a judgment set: what appears in
// annotations.<name>.yaml, qrels.<name>.tsv, --judgments and
// spec.defaults.judgments.
//
// Heuristic and manual judges name themselves. LLM judges are named by their
// provider ("claude-cli"), not by the strategy — a single "llm" name would
// collide the moment a second provider grades the same track.
func JudgmentSetName(kind StrategyKind, provider Provider) string {
	if kind != StrategyLLM {
		return string(kind)
	}
	if provider == "" {
		provider = DefaultProvider
	}
	return string(provider)
}

// ParseSelector resolves a judgment-set name back into the strategy and
// provider that produced it. This is the read path — --judgments,
// spec.defaults.judgments, bench export --strategy — and is the exact inverse
// of JudgmentSetName.
//
// Writing is separate and explicit: bench judge takes --strategy llm
// --provider claude-cli.
func ParseSelector(name string) (StrategyKind, Provider, error) {
	switch kind := StrategyKind(name); kind {
	case StrategyKeyword, StrategyBM25, StrategyVector, StrategyHybrid, StrategyManual:
		return kind, "", nil
	case StrategyLLM:
		// "llm" alone does not name a judgment set — every LLM set is filed
		// under its provider.
		return "", "", fmt.Errorf(
			"%q does not name a judgment set — use a provider (known: %s)",
			name, strings.Join(KnownProviders(), ", "))
	}

	if _, ok := providers[Provider(name)]; ok {
		return StrategyLLM, Provider(name), nil
	}

	return "", "", fmt.Errorf("unknown judgment set %q (known: %s)", name, strings.Join(KnownStrategies(), ", "))
}

// KnownJudgeStrategies returns the values bench judge accepts in --strategy.
// Distinct from KnownStrategies, which lists judgment-set names on disk: llm is
// writable but never a set name, and providers are set names but not strategies.
func KnownJudgeStrategies() []string {
	return []string{
		string(StrategyKeyword),
		string(StrategyBM25),
		string(StrategyVector),
		string(StrategyHybrid),
		string(StrategyLLM),
		string(StrategyManual),
	}
}

// KnownStrategies returns every judgment-set name the spec validator accepts.
// LLM entries come from the provider registry, so a newly registered vendor is
// immediately valid without touching this list.
func KnownStrategies() []string {
	out := []string{
		string(StrategyKeyword),
		string(StrategyBM25),
		string(StrategyVector),
		string(StrategyHybrid),
	}
	out = append(out, KnownProviders()...)
	return append(out, string(StrategyManual))
}
