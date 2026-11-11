package judgment

import (
	"context"

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

	titleTokens := termSet(doc.Title)
	descTokens := termSet(doc.Description)
	contentTokens := termSet(doc.Content)

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
