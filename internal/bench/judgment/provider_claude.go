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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
	binary  string
	version string
	model   string
	effort  Effort
}

func newClaudeCLIProvider(opts StrategyOptions) (LLMProvider, error) {
	model, err := requireClaudeModel(ProviderClaudeCLI, opts.Model)
	if err != nil {
		return nil, err
	}
	if err := requireEffort(ProviderClaudeCLI, opts.Effort); err != nil {
		return nil, err
	}
	name := opts.CLIBinary
	if name == "" {
		name = claudeDefaultBinary
	}
	bin, err := resolveCLIBinary(name)
	if err != nil {
		return nil, err
	}
	version, err := claudeCLIVersion(bin)
	if err != nil {
		return nil, err
	}
	return &claudeCLIProvider{binary: bin, version: version, model: model, effort: opts.Effort}, nil
}

// resolveCLIBinary finds the file name runs, through PATH and any symlink, once:
// an auto-update re-points the claude link, and a run that looked it up per
// grade could grade under a version the judge block does not name.
func resolveCLIBinary(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("find the claude CLI %s: %w", name, err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve the claude CLI %s: %w", path, err)
	}
	return filepath.Abs(resolved)
}

// claudeCLIVersion is what the CLI reports with --version, so the judge block
// names the CLI that grades rather than where it is installed.
func claudeCLIVersion(binary string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", binary, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *claudeCLIProvider) Name() Provider { return ProviderClaudeCLI }
func (p *claudeCLIProvider) Model() string  { return p.model }
func (p *claudeCLIProvider) Effort() Effort { return p.effort }

func (p *claudeCLIProvider) Invocation() string {
	args := p.args("<system-prompt>")
	words := make([]string, 0, len(args)+3)
	words = append(words, claudeCLIDisableAdvisor, "CLAUDE_CODE_MAX_OUTPUT_TOKENS=<max-output-tokens>", claudeDefaultBinary)
	for _, arg := range args {
		if arg == "" {
			arg = `""`
		}
		words = append(words, arg)
	}
	return claudeDefaultBinary + " --version " + p.version + "; " + strings.Join(words, " ")
}

func (p *claudeCLIProvider) args(system string) []string {
	args := append([]string{"-p"}, claudeCLIIsolation...)
	return append(args, "--output-format", "json", "--model", p.model, "--effort", string(p.effort), "--system-prompt", system)
}

// Complete runs the CLI from an empty directory of its own, so it finds no
// project files to load, and sends the user payload on stdin.
func (p *claudeCLIProvider) Complete(ctx context.Context, req CompletionRequest) (string, error) {
	workDir, err := p.requestWorkDir(req)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(workDir)

	cmd := exec.CommandContext(ctx, p.binary, p.args(req.System)...)
	cmd.Dir = workDir
	cmd.Env = claudeCLIEnv(req.MaxTokens)
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
	if others := result.modelsOtherThan(p.model); len(others) > 0 {
		return "", fmt.Errorf("%s answered with %s, not the pinned %s", p.binary, strings.Join(others, ", "), p.model)
	}
	return result.Result, nil
}

// claudeCLIGradingOverrides are variables the CLI reads ahead of its flags to
// change the effort, the thinking, the output budget or the request body a
// grade is made under.
var claudeCLIGradingOverrides = []string{
	"CLAUDE_CODE_EFFORT_LEVEL",
	"CLAUDE_CODE_MAX_OUTPUT_TOKENS",
	"MAX_THINKING_TOKENS",
	"CLAUDE_CODE_DISABLE_THINKING",
	"CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING",
	"CLAUDE_CODE_EXTRA_BODY",
}

// claudeCLIEnv is the parent's environment without the grading overrides,
// with the advisor off and the judge's output budget.
func claudeCLIEnv(maxOutputTokens int) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(claudeCLIGradingOverrides, name)
	})
	return append(env, claudeCLIDisableAdvisor, "CLAUDE_CODE_MAX_OUTPUT_TOKENS="+strconv.Itoa(maxOutputTokens))
}

// requestWorkDir creates an empty directory named after req and the model and
// effort grading it: the CLI tells the model its working directory, so a
// random name would change every request, and two judges grading one pool
// side by side would share one directory.
func (p *claudeCLIProvider) requestWorkDir(req CompletionRequest) (string, error) {
	sum := sha256.Sum256([]byte(strings.Join([]string{p.model, string(p.effort), req.System, req.User}, "\x00")))
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
	// ModelUsage is keyed by every model that answered, a fallback included.
	ModelUsage map[string]struct {
		CanonicalModel string `json:"canonicalModel"`
	} `json:"modelUsage"`
}

func (r claudeCLIResult) modelsOtherThan(pinned string) []string {
	var others []string
	for model, usage := range r.ModelUsage {
		if model != pinned && usage.CanonicalModel != pinned {
			others = append(others, model)
		}
	}
	slices.Sort(others)
	return others
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
		// Long enough for a reply that spends the whole output budget.
		client: &http.Client{Timeout: 10 * time.Minute},
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

// messagesRequest leaves out the sampling parameters, which current models
// reject, and thinking, since Opus 5.5 rejects it disabled: the model thinks
// adaptively at the pinned effort.
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
