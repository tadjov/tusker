package judgment

import (
	"context"
	"math"

	"github.com/tadjov/tusker/internal/bench/meta"
)

const (
	bm25K1            = 1.5
	bm25B             = 0.75
	bm25RubricVersion = "v1"
)

// BM25Strategy grades candidates with Okapi BM25 computed over the per-query
// candidate pool as a local corpus: idf from pool document frequencies, avgdl
// from pool documents. Each doc's score is normalised against the top-scoring
// candidate and mapped to a grade 0-3.
//
// It is a heuristic, no-network judge — stronger than the keyword judge's token overlap
// because it rewards rarer terms and saturates term frequency, but still purely
// keyword-based with no semantic understanding. Use it as a reproducible
// baseline alongside the LLM judges.
type BM25Strategy struct{}

func NewBM25Strategy() *BM25Strategy { return &BM25Strategy{} }

func (BM25Strategy) Name() string { return string(StrategyBM25) }

func (BM25Strategy) Describe() meta.Judge {
	return meta.Judge{Strategy: string(StrategyBM25), RubricVersion: bm25RubricVersion}
}

// GradePool gives 0 to a document the query's category gate refuses; the term
// statistics still count it.
func (s BM25Strategy) GradePool(_ context.Context, q GradingQuery, docs []GradingDoc) ([]GradedDoc, error) {
	tq, err := readTermQuery(q)
	if err != nil {
		return nil, err
	}
	analyzed := analyzeDocs(docs)
	norms := s.normScores(tq.terms, analyzed)
	out := make([]GradedDoc, len(docs))
	for i, d := range docs {
		grade := GradeNotRelev
		if tq.admits(analyzed[i]) {
			grade = gradeFromNorm(norms[i])
		}
		out[i] = GradedDoc{DocID: d.ID, Grade: grade}
	}
	return out, nil
}

func analyzeDocs(docs []GradingDoc) []analyzedDoc {
	out := make([]analyzedDoc, len(docs))
	for i, d := range docs {
		out[i] = analyzeDoc(d)
	}
	return out
}

// normScores returns each doc's BM25 score normalised to [0,1] against the
// top-scoring candidate in the pool. Shared with HybridStrategy.
func (BM25Strategy) normScores(terms []string, docs []analyzedDoc) []float64 {
	norms := make([]float64, len(docs))
	if len(docs) == 0 {
		return norms
	}

	counts := make([]map[string]int, len(docs))
	lengths := make([]int, len(docs))
	df := map[string]int{}
	totalLen := 0
	for i, d := range docs {
		c := map[string]int{}
		for _, field := range d.fields() {
			for _, t := range field {
				c[t]++
			}
		}
		counts[i] = c
		for _, n := range c {
			lengths[i] += n
		}
		totalLen += lengths[i]
		for t := range c {
			df[t]++
		}
	}

	avgdl := float64(totalLen) / float64(len(docs))
	if avgdl == 0 {
		avgdl = 1
	}
	n := float64(len(docs))

	scores := make([]float64, len(docs))
	maxScore := 0.0
	for i := range docs {
		dl := float64(lengths[i])
		var score float64
		for _, t := range terms {
			tf := float64(counts[i][t])
			if tf == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df[t])+0.5)/(float64(df[t])+0.5))
			score += idf * (tf * (bm25K1 + 1)) / (tf + bm25K1*(1-bm25B+bm25B*dl/avgdl))
		}
		scores[i] = score
		if score > maxScore {
			maxScore = score
		}
	}

	if maxScore > 0 {
		for i := range docs {
			norms[i] = scores[i] / maxScore
		}
	}
	return norms
}

// gradeFromNorm maps a [0,1] relevance score to a grade. Shared by BM25 and the
// hybrid fusion score.
func gradeFromNorm(norm float64) int {
	switch {
	case norm >= 0.66:
		return GradeHighly
	case norm >= 0.40:
		return GradeRelevant
	case norm >= 0.15:
		return GradeMarginally
	default:
		return GradeNotRelev
	}
}
