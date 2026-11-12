package judgment

import (
	"strings"
	"testing"
)

func TestParseSelector(t *testing.T) {
	tests := []struct {
		sel      string
		kind     StrategyKind
		provider Provider
		wantErr  string
	}{
		{sel: "keyword", kind: StrategyKeyword},
		{sel: "bm25", kind: StrategyBM25},
		{sel: "vector", kind: StrategyVector},
		{sel: "hybrid", kind: StrategyHybrid},
		{sel: "manual", kind: StrategyManual},
		{sel: "claude-cli", kind: StrategyLLM, provider: ProviderClaudeCLI},
		{sel: "claude-api", kind: StrategyLLM, provider: ProviderClaudeAPI},

		// "llm" is writable but is never a judgment-set name: every LLM set is
		// filed under its provider, or two providers would collide on one file.
		{sel: "llm", wantErr: "does not name a judgment set"},

		{sel: "codex-cli", wantErr: "unknown judgment set"},
		{sel: "bogus", wantErr: "unknown judgment set"},
	}

	for _, tc := range tests {
		t.Run(tc.sel, func(t *testing.T) {
			kind, provider, err := ParseSelector(tc.sel)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseSelector(%q) = (%q, %q), want error", tc.sel, kind, provider)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSelector(%q): %v", tc.sel, err)
			}
			if kind != tc.kind || provider != tc.provider {
				t.Errorf("ParseSelector(%q) = (%q, %q), want (%q, %q)",
					tc.sel, kind, provider, tc.kind, tc.provider)
			}
		})
	}
}

// KnownStrategies is what the spec validator rejects typos against: it lists
// judgment-set NAMES, so providers appear and the bare "llm" strategy does not.
func TestKnownStrategiesListsSetNames(t *testing.T) {
	known := KnownStrategies()
	joined := strings.Join(known, ",")

	for _, want := range []string{"keyword", "bm25", "vector", "hybrid", "claude-cli", "claude-api", "manual"} {
		if !strings.Contains(joined, want) {
			t.Errorf("KnownStrategies() = %v, missing %q", known, want)
		}
	}
	for _, k := range known {
		if k == string(StrategyLLM) {
			t.Errorf("KnownStrategies() lists %q, which is not a judgment-set name", k)
		}
	}
}

func TestJudgmentSetName(t *testing.T) {
	tests := []struct {
		kind     StrategyKind
		provider Provider
		want     string
	}{
		{kind: StrategyKeyword, want: "keyword"},
		{kind: StrategyManual, want: "manual"},
		{kind: StrategyLLM, provider: ProviderClaudeCLI, want: "claude-cli"},
		{kind: StrategyLLM, provider: ProviderClaudeAPI, want: "claude-api"},
		// An unset provider falls back to the default rather than producing an
		// unnamed file.
		{kind: StrategyLLM, want: string(DefaultProvider)},
	}

	for _, tc := range tests {
		if got := JudgmentSetName(tc.kind, tc.provider); got != tc.want {
			t.Errorf("JudgmentSetName(%q, %q) = %q, want %q", tc.kind, tc.provider, got, tc.want)
		}
	}
}

// JudgmentSetName and ParseSelector must be exact inverses, or a set written by
// bench judge cannot be found again by bench run.
func TestJudgmentSetNameRoundTrips(t *testing.T) {
	cases := []struct {
		kind     StrategyKind
		provider Provider
	}{
		{kind: StrategyKeyword},
		{kind: StrategyBM25},
		{kind: StrategyVector},
		{kind: StrategyHybrid},
		{kind: StrategyManual},
		{kind: StrategyLLM, provider: ProviderClaudeCLI},
		{kind: StrategyLLM, provider: ProviderClaudeAPI},
	}

	for _, tc := range cases {
		name := JudgmentSetName(tc.kind, tc.provider)
		kind, provider, err := ParseSelector(name)
		if err != nil {
			t.Errorf("ParseSelector(%q): %v", name, err)
			continue
		}
		if kind != tc.kind || provider != tc.provider {
			t.Errorf("round trip of (%q, %q) via %q = (%q, %q)", tc.kind, tc.provider, name, kind, provider)
		}
	}
}

func TestResolveClaudeModelAliases(t *testing.T) {
	tests := []struct{ in, want string }{
		{in: "", want: DefaultJudgeModel},
		{in: "haiku", want: DefaultJudgeModel},
		{in: "HAIKU", want: DefaultJudgeModel},
		{in: "sonnet", want: "claude-sonnet-5"},
		{in: "opus", want: "claude-opus-5"},
		// A full id passes through untouched.
		{in: "claude-haiku-4-5-20251001", want: "claude-haiku-4-5-20251001"},
		{in: "some-future-model", want: "some-future-model"},
	}

	for _, tc := range tests {
		if got := resolveClaudeModel(tc.in); got != tc.want {
			t.Errorf("resolveClaudeModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLLMDescribeDefaultsToHaiku(t *testing.T) {
	for _, provider := range []Provider{ProviderClaudeCLI, ProviderClaudeAPI} {
		t.Run(string(provider), func(t *testing.T) {
			s, err := NewLLMStrategy(StrategyOptions{Provider: provider, APIKey: "test-key"})
			if err != nil {
				t.Fatalf("NewLLMStrategy: %v", err)
			}

			got := s.Describe()
			if got.Strategy != string(StrategyLLM) {
				t.Errorf("Strategy = %q, want %q", got.Strategy, StrategyLLM)
			}
			if got.Provider != string(provider) {
				t.Errorf("Provider = %q, want %q", got.Provider, provider)
			}
			if got.Model != DefaultJudgeModel {
				t.Errorf("Model = %q, want %q", got.Model, DefaultJudgeModel)
			}
			if got.PromptVersion != PromptVersion {
				t.Errorf("PromptVersion = %q, want %q", got.PromptVersion, PromptVersion)
			}
		})
	}
}

// An omitted provider must still yield a usable judge rather than an error.
func TestLLMDefaultProvider(t *testing.T) {
	s, err := NewLLMStrategy(StrategyOptions{})
	if err != nil {
		t.Fatalf("NewLLMStrategy: %v", err)
	}

	if got := s.Describe().Provider; got != string(DefaultProvider) {
		t.Errorf("Provider = %q, want %q", got, DefaultProvider)
	}
}

// --model takes an alias; the judge block must record the expanded id, since an
// alias would silently re-point at a different model later.
func TestLLMModelAliasRecordedAsFullID(t *testing.T) {
	s, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeCLI, Model: "sonnet"})
	if err != nil {
		t.Fatalf("NewLLMStrategy: %v", err)
	}

	if got := s.Describe().Model; got != "claude-sonnet-5" {
		t.Errorf("Model = %q, want %q", got, "claude-sonnet-5")
	}
}

func TestLLMBatchSizeFollowsProvider(t *testing.T) {
	cli, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeCLI})
	if err != nil {
		t.Fatalf("NewLLMStrategy(cli): %v", err)
	}
	api, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeAPI, APIKey: "k"})
	if err != nil {
		t.Fatalf("NewLLMStrategy(api): %v", err)
	}

	if cli.PreferredBatchSize() != claudeCLIBatchSize {
		t.Errorf("cli batch = %d, want %d", cli.PreferredBatchSize(), claudeCLIBatchSize)
	}
	if api.PreferredBatchSize() != claudeAPIBatchSize {
		t.Errorf("api batch = %d, want %d", api.PreferredBatchSize(), claudeAPIBatchSize)
	}
}

func TestLLMStrategyUnknownProvider(t *testing.T) {
	if _, err := NewLLMStrategy(StrategyOptions{Provider: "codex-cli"}); err == nil {
		t.Error("NewLLMStrategy with unregistered provider should fail")
	}
}
