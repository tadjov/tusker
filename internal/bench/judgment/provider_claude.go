package judgment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// DefaultJudgeModel is the model every Claude-backed judge uses when --model is
// omitted. Haiku is the default across both transports: grading is a short,
// well-specified classification task, so the cheapest capable model is the right
// baseline, and sharing one default keeps CLI- and API-graded qrels comparable.
const DefaultJudgeModel = "claude-haiku-4-5-20251001"

// claudeModelAliases lets --model take a short name. The alias is expanded at
// construction so the judge block always records a concrete model id — an
// alias would silently re-point at a different model later.
var claudeModelAliases = map[string]string{
	"haiku":  DefaultJudgeModel,
	"sonnet": "claude-sonnet-5",
	"opus":   "claude-opus-5",
	"fable":  "claude-fable-5",
}

const (
	claudeDefaultBinary  = "claude"
	claudeDefaultBaseURL = "https://api.anthropic.com"
	claudeAPIKeyEnv      = "ANTHROPIC_API_KEY"
	claudeAPIVersion     = "2023-06-01"

	claudeCLIBatchSize = 10
	claudeAPIBatchSize = 20
)

// resolveClaudeModel expands an alias, defaults an empty value, and otherwise
// passes a full model id through untouched.
func resolveClaudeModel(model string) string {
	if model == "" {
		return DefaultJudgeModel
	}
	if full, ok := claudeModelAliases[strings.ToLower(model)]; ok {
		return full
	}
	return model
}

// ClaudeModelAliases lists the accepted --model shorthands, sorted, for help
// text and error messages.
func ClaudeModelAliases() []string {
	out := make([]string, 0, len(claudeModelAliases))
	for a := range claudeModelAliases {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// claudeCLIIsolation keeps the operator's settings, CLAUDE.md, MCP servers,
// tools and skills out of every grading call. --bare would as well, but it
// reads no OAuth login, only ANTHROPIC_API_KEY.
var claudeCLIIsolation = []string{
	"--safe-mode",
	"--setting-sources", "",
	"--strict-mcp-config",
	"--tools", "",
	"--disable-slash-commands",
	"--no-session-persistence",
}

// claudeCLIDisableAdvisor turns off the advisor, a server-side tool that
// --tools "" leaves on.
const claudeCLIDisableAdvisor = "CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1"

// claudeCLIProvider drives `claude -p` as a subprocess.
type claudeCLIProvider struct {
	binary string
	model  string
}

// The error is always nil; the signature is fixed by the providers registry.
//
//nolint:unparam
func newClaudeCLIProvider(opts StrategyOptions) (LLMProvider, error) {
	bin := opts.CLIBinary
	if bin == "" {
		bin = claudeDefaultBinary
	}
	return &claudeCLIProvider{binary: bin, model: resolveClaudeModel(opts.Model)}, nil
}

func (p *claudeCLIProvider) Name() Provider          { return ProviderClaudeCLI }
func (p *claudeCLIProvider) Model() string           { return p.model }
func (p *claudeCLIProvider) PreferredBatchSize() int { return claudeCLIBatchSize }

func (p *claudeCLIProvider) Invocation() string {
	args := p.args("<system-prompt>")
	words := make([]string, 0, len(args)+2)
	words = append(words, claudeCLIDisableAdvisor, p.binary)
	for _, arg := range args {
		if arg == "" {
			arg = `""`
		}
		words = append(words, arg)
	}
	return strings.Join(words, " ")
}

func (p *claudeCLIProvider) args(system string) []string {
	args := append([]string{"-p"}, claudeCLIIsolation...)
	return append(args, "--output-format", "json", "--model", p.model, "--system-prompt", system)
}

// Complete runs the CLI from an empty directory of its own, so it finds no
// project files to load, and sends the user payload on stdin.
func (p *claudeCLIProvider) Complete(ctx context.Context, req CompletionRequest) (string, error) {
	workDir, err := os.MkdirTemp("", "bench-judge-")
	if err != nil {
		return "", fmt.Errorf("create the judge's working directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	cmd := exec.CommandContext(ctx, p.binary, p.args(req.System)...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), claudeCLIDisableAdvisor)
	cmd.Stdin = strings.NewReader(req.User)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("%s exit %d: %s %s",
				p.binary, exitErr.ExitCode(), bytes.TrimSpace(exitErr.Stderr), bytes.TrimSpace(out))
		}
		return "", fmt.Errorf("run %s: %w", p.binary, err)
	}

	var result claudeCLIResult
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("parse %s output: %w (output: %q)", p.binary, err, out)
	}
	if result.IsError {
		return "", fmt.Errorf("%s reported an error: %s", p.binary, result.Result)
	}
	return result.Result, nil
}

// claudeCLIResult is the envelope --output-format json prints.
type claudeCLIResult struct {
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// claudeAPIProvider drives the Anthropic Messages API.
type claudeAPIProvider struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

func newClaudeAPIProvider(opts StrategyOptions) (LLMProvider, error) {
	key := opts.APIKey
	if key == "" {
		key = os.Getenv(claudeAPIKeyEnv)
	}
	if key == "" {
		return nil, fmt.Errorf("provider %s: %s not set and no key provided", ProviderClaudeAPI, claudeAPIKeyEnv)
	}
	base := opts.APIBaseURL
	if base == "" {
		base = claudeDefaultBaseURL
	}
	return &claudeAPIProvider{
		apiKey:  key,
		model:   resolveClaudeModel(opts.Model),
		baseURL: base,
		client:  &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (p *claudeAPIProvider) Name() Provider          { return ProviderClaudeAPI }
func (p *claudeAPIProvider) Model() string           { return p.model }
func (p *claudeAPIProvider) PreferredBatchSize() int { return claudeAPIBatchSize }
func (p *claudeAPIProvider) Invocation() string      { return "anthropic-version: " + claudeAPIVersion }

func (p *claudeAPIProvider) Complete(ctx context.Context, cr CompletionRequest) (string, error) {
	payload, err := json.Marshal(messagesRequest{
		Model:     p.model,
		MaxTokens: cr.MaxTokens,
		System:    cr.System,
		Messages:  []messagesEntry{{Role: "user", Content: cr.User}},
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", claudeAPIVersion)
	req.Header.Set("content-type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("api request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read api response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("api status %d: %s", resp.StatusCode, string(raw))
	}

	var mr messagesResponse
	if err := json.Unmarshal(raw, &mr); err != nil {
		return "", fmt.Errorf("parse api response: %w", err)
	}
	if mr.Error != nil {
		return "", fmt.Errorf("api error %s: %s", mr.Error.Type, mr.Error.Message)
	}
	if len(mr.Content) == 0 {
		return "", fmt.Errorf("api response has no content")
	}
	return mr.Content[0].Text, nil
}

type messagesRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []messagesEntry `json:"messages"`
}

type messagesEntry struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}
