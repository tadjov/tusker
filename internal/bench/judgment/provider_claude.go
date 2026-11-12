package judgment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// claudeModelAliases lets --model take a short name for the current
// generation. The alias is expanded at construction so the judge block always
// records a concrete model id — an alias would silently re-point at a
// different model later.
var claudeModelAliases = map[string]string{
	"haiku":  "claude-haiku-5-5",
	"sonnet": "claude-sonnet-5-5",
	"opus":   "claude-opus-5-5",
	"fable":  "claude-fable-5-2",
}

const (
	claudeDefaultBinary  = "claude"
	claudeDefaultBaseURL = "https://api.anthropic.com"
	claudeAPIKeyEnv      = "ANTHROPIC_API_KEY"
	claudeAPIVersion     = "2023-06-01"
)

// requireClaudeModel resolves the model provider grades with; there is no
// default, so grades always come from a model the operator chose.
func requireClaudeModel(provider Provider, model string) (string, error) {
	if model == "" {
		return "", fmt.Errorf("provider %s needs --model to name the judge model, e.g. --model sonnet", provider)
	}
	return resolveClaudeModel(model), nil
}

// requireEffort refuses an unset effort: the model's default would grade at a
// level the judge block cannot name.
func requireEffort(provider Provider, effort Effort) error {
	if _, err := ParseEffort(string(effort)); err != nil {
		return fmt.Errorf("provider %s needs --effort: %w", provider, err)
	}
	return nil
}

// resolveClaudeModel expands an alias and passes a full model id through
// untouched.
func resolveClaudeModel(model string) string {
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
	effort Effort
}

func newClaudeCLIProvider(opts StrategyOptions) (LLMProvider, error) {
	model, err := requireClaudeModel(ProviderClaudeCLI, opts.Model)
	if err != nil {
		return nil, err
	}
	if err := requireEffort(ProviderClaudeCLI, opts.Effort); err != nil {
		return nil, err
	}
	bin := opts.CLIBinary
	if bin == "" {
		bin = claudeDefaultBinary
	}
	return &claudeCLIProvider{binary: bin, model: model, effort: opts.Effort}, nil
}

func (p *claudeCLIProvider) Name() Provider { return ProviderClaudeCLI }
func (p *claudeCLIProvider) Model() string  { return p.model }
func (p *claudeCLIProvider) Effort() Effort { return p.effort }

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
	return append(args, "--output-format", "json", "--model", p.model, "--effort", string(p.effort), "--system-prompt", system)
}

// Complete runs the CLI from an empty directory of its own, so it finds no
// project files to load, and sends the user payload on stdin.
func (p *claudeCLIProvider) Complete(ctx context.Context, req CompletionRequest) (string, error) {
	workDir, err := requestWorkDir(req)
	if err != nil {
		return "", err
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

// requestWorkDir creates an empty directory named after req: the CLI tells the
// model its working directory, so a random name would change every request.
func requestWorkDir(req CompletionRequest) (string, error) {
	sum := sha256.Sum256([]byte(req.System + "\x00" + req.User))
	dir := filepath.Join(os.TempDir(), "bench-judge-"+hex.EncodeToString(sum[:8]))
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clear the judge's working directory: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the judge's working directory: %w", err)
	}
	return dir, nil
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
	effort  Effort
	baseURL string
	client  *http.Client
}

func newClaudeAPIProvider(opts StrategyOptions) (LLMProvider, error) {
	model, err := requireClaudeModel(ProviderClaudeAPI, opts.Model)
	if err != nil {
		return nil, err
	}
	if err := requireEffort(ProviderClaudeAPI, opts.Effort); err != nil {
		return nil, err
	}
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
		model:   model,
		effort:  opts.Effort,
		baseURL: base,
		client:  &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (p *claudeAPIProvider) Name() Provider { return ProviderClaudeAPI }
func (p *claudeAPIProvider) Model() string  { return p.model }
func (p *claudeAPIProvider) Effort() Effort { return p.effort }

func (p *claudeAPIProvider) Invocation() string {
	return "anthropic-version: " + claudeAPIVersion + ", output_config.effort: " + string(p.effort)
}

func (p *claudeAPIProvider) Complete(ctx context.Context, cr CompletionRequest) (string, error) {
	payload, err := json.Marshal(messagesRequest{
		Model:        p.model,
		MaxTokens:    cr.MaxTokens,
		System:       cr.System,
		Messages:     []messagesEntry{{Role: "user", Content: cr.User}},
		OutputConfig: outputConfig{Effort: p.effort},
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
	if mr.StopReason == "refusal" {
		var category string
		if mr.StopDetails != nil {
			category = mr.StopDetails.Category
		}
		slog.Warn("judge model refused the request", "model", p.model, "category", category)
		return "", fmt.Errorf("%s refused the request (category %q)", p.model, category)
	}
	var text strings.Builder
	for _, block := range mr.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() == 0 {
		return "", fmt.Errorf("api response has no text (stop_reason %q)", mr.StopReason)
	}
	return text.String(), nil
}

// messagesRequest leaves out sampling parameters and thinking, which current
// models reject, so the model thinks adaptively at the pinned effort.
type messagesRequest struct {
	Model        string          `json:"model"`
	MaxTokens    int             `json:"max_tokens"`
	System       string          `json:"system,omitempty"`
	Messages     []messagesEntry `json:"messages"`
	OutputConfig outputConfig    `json:"output_config"`
}

type outputConfig struct {
	Effort Effort `json:"effort"`
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
	StopReason  string `json:"stop_reason"`
	StopDetails *struct {
		Category string `json:"category"`
	} `json:"stop_details"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}
