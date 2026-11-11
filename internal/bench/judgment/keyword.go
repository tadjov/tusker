package judgment

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/tadjov/tusker/internal/bench/meta"
)

// KeywordStrategy is a deterministic baseline judge. It tokenizes the query
// text, then counts how many distinct query tokens appear in the document's
// title, description, and content.
type KeywordStrategy struct{}

const keywordRubricVersion = "v1"

func NewKeywordStrategy() *KeywordStrategy { return &KeywordStrategy{} }

func (KeywordStrategy) Name() string { return string(StrategyKeyword) }

func (KeywordStrategy) Describe() meta.Judge {
	return meta.Judge{Strategy: string(StrategyKeyword), RubricVersion: keywordRubricVersion}
}

func (KeywordStrategy) Grade(_ context.Context, q GradingQuery, doc GradingDoc) (int, error) {
	terms, err := queryTerms(q)
	if err != nil {
		return GradeUnjudged, err
	}

	titleTokens := tokenSet(doc.Title)
	descTokens := tokenSet(doc.Description)
	contentTokens := tokenSet(doc.Content)

	titleHits, otherHits := 0, 0
	for _, t := range terms {
		if titleTokens[t] {
			titleHits++
			continue
		}
		if descTokens[t] || contentTokens[t] {
			otherHits++
		}
	}

	total := len(terms)
	titleRatio := float64(titleHits) / float64(total)
	overallRatio := float64(titleHits+otherHits) / float64(total)

	switch {
	case titleRatio >= 0.5:
		return GradeHighly, nil
	case overallRatio >= 0.5:
		return GradeRelevant, nil
	case overallRatio >= 0.25:
		return GradeMarginally, nil
	default:
		return GradeNotRelev, nil
	}
}

// NoQueryTermsError reports a query with no gradable terms. It is a property of
// the query, so every document fails the same way and no retry can fix it.
type NoQueryTermsError struct {
	QueryID string
}

func (e *NoQueryTermsError) Error() string {
	return fmt.Sprintf("query %q has no usable text (fix its text in the suite and re-pool, or set --strategy manual)", e.QueryID)
}

// queryTerms returns the query's distinct gradable terms, sorted.
func queryTerms(q GradingQuery) ([]string, error) {
	terms := tokenizeSlice(q.Text)
	if len(terms) == 0 {
		return nil, &NoQueryTermsError{QueryID: q.ID}
	}
	return terms, nil
}

var (
	tokenSplit = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	stopwords  = map[string]struct{}{
		"a": {}, "an": {}, "and": {}, "the": {}, "or": {}, "of": {}, "to": {},
		"in": {}, "on": {}, "at": {}, "by": {}, "for": {}, "with": {}, "from": {},
		"is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "been": {}, "being": {},
		"it": {}, "its": {}, "as": {}, "that": {}, "this": {}, "these": {}, "those": {},
		"news": {}, "article": {}, "search": {}, "query": {},
	}
)

func tokenize(s string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, raw := range tokenSplit.Split(strings.ToLower(s), -1) {
		if len(raw) < 3 {
			continue
		}
		if _, skip := stopwords[raw]; skip {
			continue
		}
		out[raw] = struct{}{}
	}
	return out
}

func tokenSet(s string) map[string]bool {
	out := make(map[string]bool)
	for _, raw := range tokenSplit.Split(strings.ToLower(s), -1) {
		if len(raw) < 3 {
			continue
		}
		out[raw] = true
	}
	return out
}
