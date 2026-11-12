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
	// contentCutRunes is the median cc-news article's length, so half the
	// corpus reaches the judge whole.
	contentCutRunes = 1744

	phraseSatisfied = `This is a phrase query, and the article contains the phrase. ` +
		`Grade how well it answers the query.`

	booleanSatisfied = `This is a boolean query, and the article satisfies the whole expression, negated terms included. ` +
		`Grade how well it answers the query.`
)

// systemPrompt is the rubric of every grading request, one doc or a batch.
const systemPrompt = `You are an information retrieval relevance judge. You score how well each candidate news article matches a user's search query.

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

Output ONLY the JSON the request asks for. Never wrap it in markdown, never add prose.`

// BuildGradingPrompt is the user payload for grading one doc.
func BuildGradingPrompt(q GradingQuery, doc GradingDoc) string {
	var sb strings.Builder
	writeQuery(&sb, q)
	fmt.Fprintf(&sb, "Article (doc_id: %s):\n", doc.ID)
	writeArticle(&sb, doc)
	fmt.Fprintf(&sb, "\nRespond with ONLY this JSON: {\"doc_id\":\"%s\",\"grade\":<0|1|2|3>}", doc.ID)
	return sb.String()
}

func writeQuery(sb *strings.Builder, q GradingQuery) {
	if note := constraintNote(q.Category); note != "" {
		sb.WriteString(note)
		sb.WriteString("\n\n")
	}
	fmt.Fprintf(sb, "Query: %s\n\n", q.Text)
}

// writeArticle shows the title and description in full and the content up to
// contentCutRunes.
func writeArticle(sb *strings.Builder, doc GradingDoc) {
	fmt.Fprintf(sb, "Title: %s\n", doc.Title)
	if doc.Description != "" {
		fmt.Fprintf(sb, "Description: %s\n", doc.Description)
	}
	if doc.Content != "" {
		fmt.Fprintf(sb, "Content: %s\n", truncateRunes(doc.Content, contentCutRunes))
	}
}

// constraintNote tells the model the article already satisfies the query's
// category constraint, which the code checked before asking.
func constraintNote(category suite.Category) string {
	switch category {
	case suite.CategoryPhrase:
		return phraseSatisfied
	case suite.CategoryBoolean:
		return booleanSatisfied
	default:
		return ""
	}
}

// gradeResponse holds Grade as a pointer so a missing or null grade stays
// distinguishable from grade 0.
type gradeResponse struct {
	DocID string `json:"doc_id"`
	Grade *int   `json:"grade"`
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
	if resp.Grade == nil {
		return 0, fmt.Errorf("doc %s has no grade (body: %q)", expectedDocID, body)
	}
	if *resp.Grade < 0 || *resp.Grade > 3 {
		return 0, fmt.Errorf("grade %d out of range [0,3]", *resp.Grade)
	}
	return *resp.Grade, nil
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
// v2 added the per-category constraints. v3 sends the query's text, notes the
// constraint the code already checked, and shares one rubric and content cut
// between single and batched requests.
const PromptVersion = "v3"

// BuildBatchGradingPrompt is the user payload for grading docs in one request:
// candidates numbered [1]..[N], answered as a JSON array in input order.
func BuildBatchGradingPrompt(q GradingQuery, docs []GradingDoc) string {
	var sb strings.Builder
	writeQuery(&sb, q)
	fmt.Fprintf(&sb, "Grade each of the %d candidate articles below.\n\n", len(docs))
	for i, d := range docs {
		fmt.Fprintf(&sb, "[%d] doc_id: %s\n", i+1, d.ID)
		writeArticle(&sb, d)
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "Return a JSON array of exactly %d entries, in the same order. ", len(docs))
	sb.WriteString(`Each entry: {"doc_id":"<uuid>","grade":<0|1|2|3>}. ` +
		`Output ONLY the JSON array. No prose, no markdown, no code fences.`)
	return sb.String()
}

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
		if e.Grade == nil || *e.Grade < 0 || *e.Grade > 3 {
			continue
		}
		byID[e.DocID] = *e.Grade
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
