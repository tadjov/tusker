package judgment

import (
	"context"
	"slices"

	"github.com/tadjov/tusker/internal/bench/meta"
)

// KeywordStrategy is a deterministic baseline judge. It tokenizes the query
// text, then counts how many distinct query tokens appear in the document's
// title, description, and content.
type KeywordStrategy struct{}

const keywordRubricVersion = "v2"

func NewKeywordStrategy() *KeywordStrategy { return &KeywordStrategy{} }

func (KeywordStrategy) Name() string { return string(StrategyKeyword) }

func (KeywordStrategy) Describe() meta.Judge {
	return meta.Judge{Strategy: string(StrategyKeyword), RubricVersion: keywordRubricVersion}
}

// Grade gives 0 to a document the query's filters or category gate refuse.
func (KeywordStrategy) Grade(_ context.Context, q GradingQuery, doc GradingDoc) (int, error) {
	tq, err := readTermQuery(q)
	if err != nil {
		return GradeUnjudged, err
	}
	analyzed := analyzeDoc(doc)
	if !tq.admits(analyzed) {
		return GradeNotRelev, nil
	}

	titleHits, otherHits := 0, 0
	for _, t := range tq.terms {
		if slices.Contains(analyzed.title, t) {
			titleHits++
			continue
		}
		if slices.Contains(analyzed.description, t) || slices.Contains(analyzed.content, t) {
			otherHits++
		}
	}

	total := len(tq.terms)
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
