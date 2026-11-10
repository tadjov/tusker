// Package meta defines the provenance block embedded in every bench artifact.
// Mirror of the "report card" convention from Anthropic/OpenAI eval cookbooks:
// every output should self-attest to who/what/when produced it.
package meta

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/DjordjeVuckovic/tusker/internal/bench/version"
)

// Meta is embedded in pool/annotations/report artifacts. Fields are
// intentionally additive — readers tolerate unknown keys, writers omit empty.
type Meta struct {
	RunID       string    `yaml:"run_id" json:"run_id"`
	Tool        string    `yaml:"tool" json:"tool"`
	GeneratedAt time.Time `yaml:"generated_at" json:"generated_at"`

	// Identity of the track/spec/suite that produced this artifact.
	SpecID       string `yaml:"spec_id,omitempty" json:"spec_id,omitempty"`
	SuiteID      string `yaml:"suite_id,omitempty" json:"suite_id,omitempty"`
	SuiteVersion string `yaml:"suite_version,omitempty" json:"suite_version,omitempty"`

	// Pool-only fields.
	PoolDepth         int                `yaml:"pool_depth,omitempty" json:"pool_depth,omitempty"`
	Engines           []string           `yaml:"engines,omitempty" json:"engines,omitempty"`
	DroppedExecutions []DroppedExecution `yaml:"dropped_executions,omitempty" json:"dropped_executions,omitempty"`

	// Judgment fields; a report also records its pool in PoolRunID.
	Judge          *Judge `yaml:"judge,omitempty" json:"judge,omitempty"`
	PoolRef        string `yaml:"pool_ref,omitempty" json:"pool_ref,omitempty"`
	PoolRunID      string `yaml:"pool_run_id,omitempty" json:"pool_run_id,omitempty"`
	GradedCount    *int   `yaml:"graded_count,omitempty" json:"graded_count,omitempty"`
	UnjudgedCount  *int   `yaml:"unjudged_count,omitempty" json:"unjudged_count,omitempty"`
	RelevanceScale []int  `yaml:"relevance_scale,omitempty" json:"relevance_scale,omitempty"`

	// Legacy flat judgment fields, retained read-only so annotations written
	// before the judge block still load at schema_version 1. Never written —
	// Normalize folds them into Judge on read. Drop once no such files remain.
	LegacyStrategy      string `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	LegacyJudgeModel    string `yaml:"judge_model,omitempty" json:"judge_model,omitempty"`
	LegacyPromptVersion string `yaml:"judge_prompt_version,omitempty" json:"judge_prompt_version,omitempty"`

	// Report-only fields.
	JudgmentsRunID string   `yaml:"judgments_run_id,omitempty" json:"judgments_run_id,omitempty"`
	Sources        *Sources `yaml:"sources,omitempty" json:"sources,omitempty"`
}

// DroppedExecution is a (query, engine) pair a pool written with
// --allow-partial holds no documents for, because every execution of it failed.
type DroppedExecution struct {
	QueryID string `yaml:"query_id" json:"query_id"`
	Engine  string `yaml:"engine" json:"engine"`
	Error   string `yaml:"error" json:"error"`
}

// Judge attests who graded an annotations file. Structured rather than flat so
// a judgment set names its grader unambiguously: Strategy is the generic
// taxonomy entry (lexical, bm25, vector, hybrid, llm-cli, llm-api, manual) and
// Provider/Model identify the vendor and model behind the LLM strategies. This
// is what makes LLM-graded qrels reproducible and lets the resume guard reject
// a grader swap mid-run.
type Judge struct {
	Strategy      string `yaml:"strategy" json:"strategy"`
	Provider      string `yaml:"provider,omitempty" json:"provider,omitempty"`
	Model         string `yaml:"model,omitempty" json:"model,omitempty"`
	PromptVersion string `yaml:"prompt_version,omitempty" json:"prompt_version,omitempty"`
	// RubricVersion names the grading rules of a heuristic judge, which has no prompt.
	RubricVersion string `yaml:"rubric_version,omitempty" json:"rubric_version,omitempty"`
}

// Equal reports whether two judges would produce comparable grades. Used by the
// resume guard: any difference means the existing file and the current run
// disagree on who is grading, so appending would corrupt the set.
func (j Judge) Equal(other Judge) bool { return j == other }

// String renders the judge for CLI output, e.g. "llm-cli/claude
// claude-haiku-4-5-20251001" or plain "lexical".
func (j Judge) String() string {
	s := j.Strategy
	if j.Provider != "" {
		s += "/" + j.Provider
	}
	if j.Model != "" {
		s += " " + j.Model
	}
	return s
}

// Normalize folds legacy flat judgment fields into the Judge block so readers
// only ever deal with one shape. Idempotent; safe on any artifact.
func (m *Meta) Normalize() {
	if m.Judge != nil {
		return
	}
	if m.LegacyStrategy == "" && m.LegacyJudgeModel == "" {
		return
	}
	m.Judge = &Judge{
		Strategy:      m.LegacyStrategy,
		Model:         m.LegacyJudgeModel,
		PromptVersion: m.LegacyPromptVersion,
	}
}

// Sources is embedded in a Report's meta to attest which on-disk artifacts
// were consumed to compute the metrics in that report.
type Sources struct {
	Spec       string   `yaml:"spec,omitempty" json:"spec,omitempty"`
	Suites     []string `yaml:"suites,omitempty" json:"suites,omitempty"`
	Pool       string   `yaml:"pool,omitempty" json:"pool,omitempty"`
	Judgments  string   `yaml:"judgments,omitempty" json:"judgments,omitempty"`
	JudgesPath string   `yaml:"judgments_path,omitempty" json:"judgments_path,omitempty"`
}

// New returns a freshly-stamped Meta with tool string + ISO timestamp + run id.
// The kind argument ("pool", "judge", "run") is embedded in the run id for
// human readability.
func New(kind string) Meta {
	return Meta{
		RunID:       NewRunID(kind),
		Tool:        version.Tool(),
		GeneratedAt: time.Now().UTC(),
	}
}

// NewRunID generates a sortable identifier of the form
//
//	2026-05-21T14-08-55-judge-7c91a3
//
// Lexicographic order = chronological order. Random suffix avoids collisions
// when two runs of the same kind fire in the same second.
func NewRunID(kind string) string {
	stamp := time.Now().UTC().Format("2006-01-02T15-04-05")
	return fmt.Sprintf("%s-%s-%s", stamp, kind, randHex(3))
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Cryptographic randomness should never fail in practice; if it does,
		// fall back to a fixed marker so the run id is still well-formed.
		return "000000"[:n*2]
	}
	return hex.EncodeToString(b)
}
