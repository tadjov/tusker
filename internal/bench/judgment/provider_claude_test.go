package judgment

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/meta"
	"github.com/tadjov/tusker/internal/bench/pool"
	"github.com/tadjov/tusker/internal/types/document"
)

// The claude CLI grades from an empty directory of its own, with the
// operator's settings, CLAUDE.md, MCP servers, tools, skills and advisor off,
// and the judge block records the command it ran.
func TestClaudeCLIProvider_GradesIsolatedAndRecordsItsInvocation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a POSIX shell script")
	}
	docID := uuid.New()
	cli := newFakeClaudeCLI(t, `{"doc_id":"`+docID.String()+`","grade":2}`)
	s, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeCLI, CLIBinary: cli.path, Model: "claude-haiku-5-5", Effort: EffortLow})
	require.NoError(t, err)

	grade, err := s.Grade(context.Background(),
		GradingQuery{ID: "q1", Text: "climate change"},
		GradingDoc{ID: docID, Title: "Climate summit opens in Belem"})

	require.NoError(t, err)
	assert.Equal(t, 2, grade)
	call := cli.call(t)
	assert.Equal(t, "claude-haiku-5-5", call.flag(t, "--model"))
	assert.Equal(t, "low", call.flag(t, "--effort"))
	for _, isolating := range []string{"--safe-mode", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence"} {
		assert.Contains(t, call.args, isolating)
	}
	assert.Empty(t, call.flag(t, "--setting-sources"), "no user, project or local settings")
	assert.Empty(t, call.flag(t, "--tools"), "no tools")
	assert.Equal(t, "1", call.disableAdvisor, "the advisor is a server-side tool --tools leaves on")

	system := call.flag(t, "--system-prompt")
	assert.NotEmpty(t, system)
	assert.Contains(t, call.stdin, "Climate summit opens in Belem")
	assert.NotContains(t, call.stdin, system, "the system prompt goes through its flag, not into the message")

	workDir, err := os.Getwd()
	require.NoError(t, err)
	assert.NotEqual(t, workDir, call.cwd)
	assert.Empty(t, call.cwdListing, "nothing in the working directory for the CLI to discover")
	assert.NoDirExists(t, call.cwd, "the working directory is removed after the call")

	want := []string{"claude", "--version", "2.1.322", "(Claude", "Code);",
		"CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1", "CLAUDE_CODE_MAX_OUTPUT_TOKENS=<max-output-tokens>", "claude"}
	for _, arg := range call.args {
		switch arg {
		case "":
			arg = `""`
		case system:
			arg = "<system-prompt>"
		}
		want = append(want, arg)
	}
	assert.Equal(t, want, strings.Fields(s.Describe().Invocation))
}

// The CLI tells the model its working directory, so the same request runs
// from the same directory every time, and another request from another.
func TestClaudeCLIProvider_RunsTheSameRequestFromTheSameDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a POSIX shell script")
	}
	doc := GradingDoc{ID: uuid.New(), Title: "Climate summit opens in Belem"}
	cli := newFakeClaudeCLI(t, `{"doc_id":"`+doc.ID.String()+`","grade":2}`)
	s, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeCLI, CLIBinary: cli.path, Model: "claude-haiku-5-5", Effort: EffortLow})
	require.NoError(t, err)
	gradeFrom := func(text string) string {
		t.Helper()
		_, err := s.Grade(context.Background(), GradingQuery{ID: "q1", Text: text}, doc)
		require.NoError(t, err)
		call := cli.call(t)
		assert.Empty(t, call.cwdListing)
		assert.NoDirExists(t, call.cwd)
		return call.cwd
	}

	first, again, other := gradeFrom("climate summit"), gradeFrom("climate summit"), gradeFrom("Belem")

	assert.Equal(t, first, again)
	assert.NotEqual(t, first, other)
}

// The parent's environment cannot change the effort, thinking, output budget
// or request body the judge block describes: the CLI reads them from these
// variables ahead of its flags.
func TestClaudeCLIProvider_GradesUnderTheJudgesBudgetWhateverTheParentEnvSays(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a POSIX shell script")
	}
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "max")
	t.Setenv("CLAUDE_CODE_MAX_OUTPUT_TOKENS", "100")
	t.Setenv("MAX_THINKING_TOKENS", "100")
	t.Setenv("CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_THINKING", "1")
	t.Setenv("CLAUDE_CODE_EXTRA_BODY", `{"output_config":{"effort":"max"},"temperature":1}`)
	doc := GradingDoc{ID: uuid.New(), Title: "Climate summit opens in Belem"}
	cli := newFakeClaudeCLI(t, `{"doc_id":"`+doc.ID.String()+`","grade":2}`)
	s, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeCLI, CLIBinary: cli.path, Model: "claude-haiku-5-5", Effort: EffortLow})
	require.NoError(t, err)

	_, err = s.Grade(context.Background(), GradingQuery{ID: "q1", Text: "climate summit"}, doc)

	require.NoError(t, err)
	env := cli.call(t).env
	assert.Equal(t, 16000, s.Describe().MaxOutputTokens, "room for thinking at every effort")
	assert.Equal(t, "16000", env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"])
	for _, overriding := range []string{
		"CLAUDE_CODE_EFFORT_LEVEL", "MAX_THINKING_TOKENS", "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING",
		"CLAUDE_CODE_DISABLE_THINKING", "CLAUDE_CODE_EXTRA_BODY",
	} {
		_, inherited := env[overriding]
		assert.False(t, inherited, "%s reaches the CLI", overriding)
	}
}

// The judge block names the CLI by the version it reports, not by where it
// is installed: the same version under another path is the same judge, and
// an upgraded CLI is not.
func TestClaudeCLIProvider_RecordsTheCLIVersionNotItsPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a POSIX shell script")
	}
	describe := func(version string) meta.Judge {
		t.Helper()
		cli := newFakeClaudeCLI(t, "")
		cli.version = version
		cli.write(t, "", 0)
		s, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeCLI, CLIBinary: cli.path, Model: "sonnet", Effort: EffortLow})
		require.NoError(t, err)
		return s.Describe()
	}

	installed := describe("2.1.322 (Claude Code)")

	assert.Equal(t, installed, describe("2.1.322 (Claude Code)"), "same version under another path")
	assert.NotEqual(t, installed, describe("2.1.330 (Claude Code)"), "an upgraded CLI")
	assert.Contains(t, installed.Invocation, "2.1.322 (Claude Code)")
}

func TestClaudeCLIProvider_FailsWithoutACLI(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-claude-here")

	_, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeCLI, CLIBinary: missing, Model: "sonnet", Effort: EffortLow})

	require.Error(t, err)
	assert.Contains(t, err.Error(), missing)
}

// The CLI that answered --version grades the whole run: an auto-update that
// re-points the claude link mid-run does not move later grades to a version
// the judge block does not name.
func TestClaudeCLIProvider_GradesOnTheBinaryItRecordedTheVersionOf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a POSIX shell script")
	}
	doc := GradingDoc{ID: uuid.New(), Title: "Floods reach Belgrade"}
	reply := `{"doc_id":"` + doc.ID.String() + `","grade":2}`
	installed, updated := newFakeClaudeCLI(t, reply), newFakeClaudeCLI(t, reply)
	updated.version = "2.1.330 (Claude Code)"
	updated.answer(t, reply)
	link := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.Symlink(installed.path, link))
	s, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeCLI, CLIBinary: link, Model: "sonnet", Effort: EffortLow})
	require.NoError(t, err)
	grade := func() {
		t.Helper()
		_, err := s.Grade(context.Background(), GradingQuery{ID: "q1", Text: "floods"}, doc)
		require.NoError(t, err)
	}

	grade()
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(updated.path, link))
	grade()

	assert.True(t, installed.graded(), "the recorded CLI grades")
	assert.False(t, updated.graded(), "the CLI the link now points at never grades")
	assert.Contains(t, s.Describe().Invocation, "2.1.322 (Claude Code)")
}

// A grade counts only when the pinned model made it: an answer the CLI got
// from any other model leaves the doc unjudged.
func TestClaudeCLIProvider_LeavesADocAnotherModelAnsweredUnjudged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a POSIX shell script")
	}
	tests := []struct {
		name     string
		answered []string
		want     int
	}{
		{name: "the pinned model", answered: []string{"claude-sonnet-5-5"}, want: GradeRelevant},
		{name: "a fallback model", answered: []string{"claude-opus-4-8"}, want: GradeUnjudged},
		{name: "the pinned model and another", answered: []string{"claude-sonnet-5-5", "claude-opus-4-8"}, want: GradeUnjudged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := document.Article{ID: uuid.New(), Title: "Floods reach Belgrade"}
			cli := newFakeClaudeCLI(t, "")
			cli.answer(t, `{"doc_id":"`+doc.ID.String()+`","grade":2}`, tt.answered...)
			s, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeCLI, CLIBinary: cli.path, Model: "sonnet", Effort: EffortLow})
			require.NoError(t, err)
			entry := pool.PoolEntry{QueryID: "q1", QueryText: "floods", Docs: []pool.PooledDoc{{DocID: doc.ID}}}

			jf, err := NewRunner(RunnerConfig{Strategy: s, Reader: &stubStorageReader{articles: map[uuid.UUID]document.Article{doc.ID: doc}}}).
				Run(context.Background(), &pool.PoolFile{Queries: []pool.PoolEntry{entry}})

			require.NoError(t, err)
			assert.Equal(t, tt.want, gradesOf(jf)[doc.ID])
		})
	}
}

// Two judges grading one pool side by side, as a comparison of models or
// efforts does, never share a working directory.
func TestClaudeCLIProvider_RunsAnotherModelOrEffortFromAnotherDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a POSIX shell script")
	}
	doc := GradingDoc{ID: uuid.New(), Title: "Climate summit opens in Belem"}
	cli := newFakeClaudeCLI(t, `{"doc_id":"`+doc.ID.String()+`","grade":2}`)
	gradeFrom := func(model string, effort Effort) string {
		t.Helper()
		s, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeCLI, CLIBinary: cli.path, Model: model, Effort: effort})
		require.NoError(t, err)
		_, err = s.Grade(context.Background(), GradingQuery{ID: "q1", Text: "climate summit"}, doc)
		require.NoError(t, err)
		return cli.call(t).cwd
	}

	sonnet := gradeFrom("sonnet", EffortLow)

	assert.Equal(t, sonnet, gradeFrom("sonnet", EffortLow))
	assert.NotEqual(t, sonnet, gradeFrom("opus", EffortLow), "another model")
	assert.NotEqual(t, sonnet, gradeFrom("sonnet", EffortHigh), "another effort")
}

// --model takes an alias or a lowercase claude- id the CLI answers under as
// is. Anything else would be recorded under an id the CLI does not report, so
// every grade it paid for would be discarded.
func TestClaudeProviders_TakeOnlyModelIDsTheCLIAnswersUnder(t *testing.T) {
	accepted := map[string]string{
		"sonnet":                    "claude-sonnet-5-5",
		"HAIKU":                     "claude-haiku-5-5",
		"claude-sonnet-5-5":         "claude-sonnet-5-5",
		"claude-haiku-4-5-20251001": "claude-haiku-4-5-20251001",
	}
	refused := []string{"sonnet[1m]", "claude-sonnet-5-5[1m]", "opusplan", "Claude-Sonnet-5-5", "some-future-model"}
	for _, provider := range []Provider{ProviderClaudeCLI, ProviderClaudeAPI} {
		for model, want := range accepted {
			t.Run(string(provider)+"/accepts "+model, func(t *testing.T) {
				s, err := NewLLMStrategy(StrategyOptions{Provider: provider, APIKey: "test-key", CLIBinary: versionOnlyClaudeCLI(t), Model: model, Effort: EffortLow})
				require.NoError(t, err)
				assert.Equal(t, want, s.Describe().Model)
			})
		}
		for _, model := range refused {
			t.Run(string(provider)+"/refuses "+model, func(t *testing.T) {
				_, err := NewLLMStrategy(StrategyOptions{Provider: provider, APIKey: "test-key", CLIBinary: versionOnlyClaudeCLI(t), Model: model, Effort: EffortLow})
				require.Error(t, err)
				assert.Contains(t, err.Error(), "--model")
			})
		}
	}
}

func TestClaudeCLIProvider_ReportsTheErrorTheCLIReturns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a POSIX shell script")
	}
	cli := newFakeClaudeCLI(t, "")
	cli.reportError(t, "Not logged in")
	s, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeCLI, CLIBinary: cli.path, Model: "claude-haiku-5-5", Effort: EffortLow})
	require.NoError(t, err)

	_, err = s.Grade(context.Background(), GradingQuery{ID: "q1", Text: "climate change"}, GradingDoc{ID: uuid.New(), Title: "Climate"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Not logged in")
}

// fakeClaudeCLI stands in for the claude binary: it records the call it gets
// and answers with --output-format json's result envelope around a reply.
type fakeClaudeCLI struct {
	path      string
	recordDir string
	version   string
}

func newFakeClaudeCLI(t *testing.T, reply string) *fakeClaudeCLI {
	t.Helper()
	cli := &fakeClaudeCLI{recordDir: t.TempDir(), version: "2.1.322 (Claude Code)"}
	cli.path = filepath.Join(t.TempDir(), "claude")
	envelope, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": reply})
	require.NoError(t, err)
	cli.write(t, string(envelope), 0)
	return cli
}

// answer makes the CLI reply with reply, reporting in modelUsage that models
// answered.
func (c *fakeClaudeCLI) answer(t *testing.T, reply string, models ...string) {
	t.Helper()
	usage := map[string]any{}
	for _, model := range models {
		usage[model] = map[string]any{"inputTokens": 10, "outputTokens": 5, "canonicalModel": model}
	}
	envelope, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": reply, "modelUsage": usage})
	require.NoError(t, err)
	c.write(t, string(envelope), 0)
}

func (c *fakeClaudeCLI) reportError(t *testing.T, message string) {
	t.Helper()
	envelope, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": message})
	require.NoError(t, err)
	c.write(t, string(envelope), 1)
}

func (c *fakeClaudeCLI) write(t *testing.T, stdout string, exitCode int) {
	t.Helper()
	record := func(name string) string { return filepath.Join(c.recordDir, name) }
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "` + c.version + `"
  exit 0
fi
printf '%s\0' "$@" > "` + record("args") + `"
pwd -P > "` + record("cwd") + `"
ls -A > "` + record("listing") + `"
printf '%s' "${CLAUDE_CODE_DISABLE_ADVISOR_TOOL-unset}" > "` + record("advisor") + `"
env > "` + record("env") + `"
cat > "` + record("stdin") + `"
cat <<'JSON'
` + stdout + `
JSON
exit ` + strconv.Itoa(exitCode) + `
`
	require.NoError(t, os.WriteFile(c.path, []byte(script), 0o755))
}

type fakeCLICall struct {
	args           []string
	cwd            string
	cwdListing     string
	disableAdvisor string
	env            map[string]string
	stdin          string
}

// graded reports whether the CLI was run to grade, not only asked its version.
func (c *fakeClaudeCLI) graded() bool {
	_, err := os.Stat(filepath.Join(c.recordDir, "args"))
	return err == nil
}

func (c *fakeClaudeCLI) call(t *testing.T) fakeCLICall {
	t.Helper()
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(c.recordDir, name))
		require.NoError(t, err)
		return string(b)
	}
	args := bytes.Split(bytes.TrimSuffix([]byte(read("args")), []byte{0}), []byte{0})
	call := fakeCLICall{
		cwd:            strings.TrimSpace(read("cwd")),
		cwdListing:     read("listing"),
		disableAdvisor: read("advisor"),
		stdin:          read("stdin"),
	}
	for _, a := range args {
		call.args = append(call.args, string(a))
	}
	call.env = map[string]string{}
	for _, line := range strings.Split(read("env"), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			call.env[name] = value
		}
	}
	return call
}

func (c fakeCLICall) flag(t *testing.T, name string) string {
	t.Helper()
	for i, a := range c.args {
		if a == name && i+1 < len(c.args) {
			return c.args[i+1]
		}
	}
	t.Fatalf("flag %s with a value not in %q", name, c.args)
	return ""
}

// A refused request has no grade to give, whatever text came before the
// refusal: the doc stays unjudged for a retry, never graded 0.
func TestClaudeAPIProvider_LeavesARefusedDocUnjudged(t *testing.T) {
	doc := document.Article{ID: uuid.New(), Title: "Gunmen attack a village"}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		partial, err := json.Marshal(`{"doc_id":"` + doc.ID.String() + `","grade":0}`)
		assert.NoError(t, err)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":` + string(partial) + `}],` +
			`"stop_reason":"refusal","stop_details":{"type":"refusal","category":"general_harms","explanation":"declined"}}`))
	}))
	defer api.Close()
	s, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeAPI, APIKey: "test-key", APIBaseURL: api.URL, Model: "claude-sonnet-5-5", Effort: EffortLow})
	require.NoError(t, err)
	entry := pool.PoolEntry{QueryID: "q1", QueryText: "village attack", Docs: []pool.PooledDoc{{DocID: doc.ID}}}

	jf, err := NewRunner(RunnerConfig{Strategy: s, Reader: &stubStorageReader{articles: map[uuid.UUID]document.Article{doc.ID: doc}}}).
		Run(context.Background(), &pool.PoolFile{Queries: []pool.PoolEntry{entry}})

	require.NoError(t, err)
	assert.Equal(t, GradeUnjudged, gradesOf(jf)[doc.ID])
}

// A grade request pins the model and the effort and sends nothing current
// models reject: no sampling parameters, no thinking setting, no prefill. The
// grade is read from the text block, after any thinking block.
func TestClaudeAPIProvider_SendsThePinnedModelAndEffortOnly(t *testing.T) {
	docID := uuid.New()
	var body map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		reply, err := json.Marshal(`{"doc_id":"` + docID.String() + `","grade":2}`)
		assert.NoError(t, err)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","stop_reason":"end_turn","content":[` +
			`{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":` + string(reply) + `}]}`))
	}))
	defer api.Close()
	s, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeAPI, APIKey: "test-key", APIBaseURL: api.URL, Model: "sonnet", Effort: EffortLow})
	require.NoError(t, err)

	grade, err := s.Grade(context.Background(), GradingQuery{ID: "q1", Text: "floods"}, GradingDoc{ID: docID, Title: "Floods reach Belgrade"})

	require.NoError(t, err)
	assert.Equal(t, GradeRelevant, grade)
	assert.Equal(t, "claude-sonnet-5-5", body["model"])
	assert.InDelta(t, 16000, body["max_tokens"], 0, "the one-doc budget")
	assert.Equal(t, 16000, s.Describe().MaxOutputTokens)
	assert.Equal(t, map[string]any{"effort": "low"}, body["output_config"])
	for _, rejected := range []string{"temperature", "top_p", "top_k", "thinking", "fallbacks"} {
		assert.NotContains(t, body, rejected)
	}
	messages, ok := body["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 1, "a user message and no assistant prefill")
	assert.Equal(t, "user", messages[0].(map[string]any)["role"])
	assert.Equal(t, "low", s.Describe().Effort)
	assert.Contains(t, s.Describe().Invocation, "low")
}

// versionOnlyClaudeCLI is a stand-in claude binary for tests that build a CLI
// judge without grading: it answers --version and fails anything else.
func versionOnlyClaudeCLI(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n[ \"$1\" = \"--version\" ] && echo \"2.1.322 (Claude Code)\" && exit 0\nexit 1\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// A batch gets its own budget, kept under the ~21,333 max_tokens a request may
// ask for without streaming, and the judge block records it with the batching.
func TestClaudeAPIProvider_SendsABatchUnderTheBatchBudget(t *testing.T) {
	docs := []GradingDoc{{ID: uuid.New(), Title: "Floods reach Belgrade"}, {ID: uuid.New(), Title: "Sava rises"}}
	var body map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		reply, err := json.Marshal(`[{"doc_id":"` + docs[0].ID.String() + `","grade":2},{"doc_id":"` + docs[1].ID.String() + `","grade":1}]`)
		assert.NoError(t, err)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":` + string(reply) + `}]}`))
	}))
	defer api.Close()
	s, err := NewLLMStrategy(StrategyOptions{Provider: ProviderClaudeAPI, APIKey: "test-key", APIBaseURL: api.URL, Model: "sonnet", Effort: EffortLow})
	require.NoError(t, err)

	_, err = s.GradeBatch(context.Background(), GradingQuery{ID: "q1", Text: "floods"}, docs)

	require.NoError(t, err)
	assert.InDelta(t, 20000, body["max_tokens"], 0)
	assert.Equal(t, &meta.Batching{Size: 2, Seed: 3, MaxOutputTokens: 20000}, RunnerConfig{Strategy: s, BatchSize: 2, BatchSeed: 3}.Judge().Batching)
}
