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

	want := []string{"CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1", cli.path}
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
}

func newFakeClaudeCLI(t *testing.T, reply string) *fakeClaudeCLI {
	t.Helper()
	cli := &fakeClaudeCLI{recordDir: t.TempDir()}
	cli.path = filepath.Join(t.TempDir(), "claude")
	envelope, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": reply})
	require.NoError(t, err)
	cli.write(t, string(envelope), 0)
	return cli
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
printf '%s\0' "$@" > "` + record("args") + `"
pwd -P > "` + record("cwd") + `"
ls -A > "` + record("listing") + `"
printf '%s' "${CLAUDE_CODE_DISABLE_ADVISOR_TOOL-unset}" > "` + record("advisor") + `"
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
	stdin          string
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
		require.NoError(t, err)
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
