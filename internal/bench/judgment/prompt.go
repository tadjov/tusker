package judgment

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/bench/suite"
)

const (
	contentSnippetRunes = 1200

	gradingScale = `Scale: 3=Highly relevant (centrally about the topic), 2=Relevant (clearly covers the topic), 1=Marginal (mentioned but not the focus), 0=Not relevant.
When in doubt between two grades, pick the lower one.
Grade what the article is ABOUT, not whether the query terms appear in the text.`

	phraseConstraint = `This is a PHRASE query. Grade 0 unless the article contains the exact phrase, ` +
		`with the words adjacent and in order. An article on the same topic that does not contain the phrase is grade 0, ` +
		`however relevant it otherwise seems.`

	booleanConstraint = `This is a BOOLEAN query. Grade 0 unless the article satisfies the whole expression, ` +
		`including every negated term: an article containing a NOT term is grade 0. ` +
		`Topical overlap alone does not satisfy the expression.`

	systemPrompt = `You are an information retrieval relevance judge. ` +
		`You grade news articles against a user's search intent. ` +
		`Reply ONLY with the requested JSON object — no prose, no markdown.`
)

// BuildGradingPrompt produces the prompt body sent to the LLM. Same prompt for
// CLI and API transports — keep them aligned so grades stay comparable.
func BuildGradingPrompt(q GradingQuery, doc GradingDoc) string {
	var sb strings.Builder
	sb.WriteString("Grade this news article's relevance to the query.\n\n")
	sb.WriteString(gradingScale)
	sb.WriteString("\n\n")
	if c := constraintFor(q.Category); c != "" {
		sb.WriteString(c)
		sb.WriteString("\n\n")
	}
	fmt.Fprintf(&sb, "Query: %s\n\n", q.Description)
	fmt.Fprintf(&sb, "Article (doc_id: %s):\n", doc.ID)
	fmt.Fprintf(&sb, "Title: %s\n", doc.Title)
	if doc.Description != "" {
		fmt.Fprintf(&sb, "Description: %s\n", doc.Description)
	}
	if doc.Content != "" {
		fmt.Fprintf(&sb, "Content: %s\n", truncateRunes(doc.Content, contentSnippetRunes))
	}
	fmt.Fprintf(&sb, "\nRespond with ONLY this JSON: {\"doc_id\":\"%s\",\"grade\":<0|1|2|3>}", doc.ID)
	return sb.String()
}

func constraintFor(category suite.Category) string {
	switch category {
	case suite.CategoryPhrase:
		return phraseConstraint
	case suite.CategoryBoolean:
		return booleanConstraint
	default:
		return ""
	}
}

type gradeResponse struct {
	DocID string `json:"doc_id"`
	Grade int    `json:"grade"`
}

// ParseGradeJSON extracts the {"doc_id","grade"} object from a raw LLM
// response. Tolerates surrounding prose/code fences by isolating the first
// balanced {...} block.
func ParseGradeJSON(raw string, expectedDocID string) (int, error) {
	body := isolateJSON(raw)

	var resp gradeResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return 0, fmt.Errorf("parse grade response: %w (body: %q)", err, body)
	}
	if resp.DocID != expectedDocID {
		return 0, fmt.Errorf("doc_id mismatch: got %q, expected %q", resp.DocID, expectedDocID)
	}
	if resp.Grade < 0 || resp.Grade > 3 {
		return 0, fmt.Errorf("grade %d out of range [0,3]", resp.Grade)
	}
	return resp.Grade, nil
}

func isolateJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "..."
}

// PromptVersion identifies the grading rubric. Bump this manually whenever the
// system prompt or grading scale changes in a way that could shift grades.
// bench judge embeds this in the annotations meta block so you can tell which
// prompt version produced a given judgment file and catch rubric drift on resume.
//
// v2 added the per-category constraints.
const PromptVersion = "v2"

const batchContentSnippetRunes = 600

// BuildBatchGradingPrompt produces the user-turn payload for batched grading.
// The matching system prompt is BatchSystemPrompt — keep them paired.
//
// Follows the Anthropic LLM-as-judge cookbook pattern:
//   - candidates are numbered [1]..[N] in input order
//   - each shows doc_id, title, description, content snippet
//   - the model is asked for a JSON array of N entries in input order
func BuildBatchGradingPrompt(q GradingQuery, docs []GradingDoc) string {
	var sb strings.Builder
	if c := constraintFor(q.Category); c != "" {
		sb.WriteString(c)
		sb.WriteString("\n\n")
	}
	fmt.Fprintf(&sb, "Query: %s\n\n", q.Description)
	fmt.Fprintf(&sb, "Grade each of the %d candidate articles below.\n\n", len(docs))

	for i, d := range docs {
		fmt.Fprintf(&sb, "[%d] doc_id: %s\n", i+1, d.ID)
		fmt.Fprintf(&sb, "Title: %s\n", d.Title)
		if d.Description != "" {
			fmt.Fprintf(&sb, "Description: %s\n", d.Description)
		}
		if d.Content != "" {
			fmt.Fprintf(&sb, "Content: %s\n", truncateRunes(d.Content, batchContentSnippetRunes))
		}
		sb.WriteString("\n")
	}

	fmt.Fprintf(&sb, "Return a JSON array of exactly %d entries, in the same order. ", len(docs))
	sb.WriteString(`Each entry: {"doc_id":"<uuid>","grade":<0|1|2|3>}. ` +
		`Output ONLY the JSON array. No prose, no markdown, no code fences.`)
	return sb.String()
}

// BatchSystemPrompt is the system instruction for batched grading. Keeps the
// rubric in one place and out of the per-batch user payload (smaller, cacheable).
const BatchSystemPrompt = `You are an information retrieval relevance judge. You score how well each candidate news article matches a user's search intent.

Grading scale:
- 3 = Highly relevant: article is centrally about the topic
- 2 = Relevant: article clearly covers the topic
- 1 = Marginal: topic is mentioned but not the focus
- 0 = Not relevant: article is about something else

Rules:
- Grade what the article IS ABOUT, not whether the query terms literally appear.
- When in doubt between two grades, pick the lower one.
- A passing mention does NOT warrant grade 2 — that is grade 1 at most.
- Be consistent: the same article quality relative to the same query should always get the same grade.

Output format: a single JSON array, one entry per candidate, in input order.
Each entry: {"doc_id":"<uuid>","grade":<0|1|2|3>}.
Output ONLY the JSON array. Never wrap in markdown, never add prose.`

// ParseBatchGradeJSON extracts the grades for a batch response. Tolerates:
//   - surrounding prose / code fences
//   - extra fields on entries
//   - shorter responses (returns whatever was parsed; caller falls back)
//   - longer responses (truncates to expected docs by doc_id match)
//
// Returns successfully-parsed entries plus the list of doc_ids that were
// missing or invalid so the caller can retry them per-doc.
func ParseBatchGradeJSON(raw string, expected []GradingDoc) (parsed []GradedDoc, missing []uuid.UUID, err error) {
	body := isolateJSONArray(raw)
	if body == "" {
		return nil, idsOf(expected), fmt.Errorf("no JSON array found in response")
	}

	var entries []gradeResponse
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		return nil, idsOf(expected), fmt.Errorf("parse batch response: %w", err)
	}

	byID := make(map[string]int, len(entries))
	for _, e := range entries {
		if e.Grade < 0 || e.Grade > 3 {
			continue
		}
		byID[e.DocID] = e.Grade
	}

	parsed = make([]GradedDoc, 0, len(expected))
	for _, d := range expected {
		if g, ok := byID[d.ID.String()]; ok {
			parsed = append(parsed, GradedDoc{DocID: d.ID, Grade: g})
			continue
		}
		missing = append(missing, d.ID)
	}
	return parsed, missing, nil
}

func isolateJSONArray(s string) string {
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

func idsOf(docs []GradingDoc) []uuid.UUID {
	out := make([]uuid.UUID, len(docs))
	for i, d := range docs {
		out[i] = d.ID
	}
	return out
}
