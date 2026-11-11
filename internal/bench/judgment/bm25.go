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
// from pool documents. A doc's score is divided by the bound no score reaches,
// Σ (k1+1)·idf over the query's terms, so its grade depends on the query and
// the pool's term statistics, never on how well another doc scores.
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
	scores := bm25Score(tq.terms, analyzed)
	out := make([]GradedDoc, len(docs))
	for i, d := range docs {
		grade := GradeNotRelev
		if tq.admits(analyzed[i]) {
			grade = gradeFromBM25Norm(scores.docs[i] / scores.bound)
		}
		out[i] = GradedDoc{DocID: d.ID, Grade: grade}
	}
	return out, nil
}

// At a doc's average length, a term occurring tf times scores
// idf·(k1+1)·tf/(tf+k1), so a doc holding every query term tf times has the
// norm tf/(tf+k1) whatever the idfs: 0.40 once, 0.57 twice, 0.67 three times.
// A doc holding only some terms once has 0.40 times their share of Σ idf: 0.20
// for one of two equally rare terms, 0.13 for one of three.
const (
	bm25HighlyNorm     = 0.50 // every term twice, not once
	bm25RelevantNorm   = 0.30 // every term once, not half of them
	bm25MarginallyNorm = 0.15 // half of the terms once, not a third
)

func gradeFromBM25Norm(norm float64) int {
	switch {
	case norm >= bm25HighlyNorm:
		return GradeHighly
	case norm >= bm25RelevantNorm:
		return GradeRelevant
	case norm >= bm25MarginallyNorm:
		return GradeMarginally
	default:
		return GradeNotRelev
	}
}

func analyzeDocs(docs []GradingDoc) []analyzedDoc {
	out := make([]analyzedDoc, len(docs))
	for i, d := range docs {
		out[i] = analyzeDoc(d)
	}
	return out
}

type bm25Scores struct {
	docs []float64
	// bound is Σ (k1+1)·idf over the query's terms, which no doc's score
	// reaches.
	bound float64
}

func bm25Score(terms []string, docs []analyzedDoc) bm25Scores {
	scores := bm25Scores{docs: make([]float64, len(docs))}
	if len(docs) == 0 {
		return scores
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
	idf := make(map[string]float64, len(terms))
	for _, t := range terms {
		idf[t] = math.Log(1 + (n-float64(df[t])+0.5)/(float64(df[t])+0.5))
		scores.bound += (bm25K1 + 1) * idf[t]
	}

	for i := range docs {
		dl := float64(lengths[i])
		for _, t := range terms {
			tf := float64(counts[i][t])
			if tf == 0 {
				continue
			}
			scores.docs[i] += idf[t] * (tf * (bm25K1 + 1)) / (tf + bm25K1*(1-bm25B+bm25B*dl/avgdl))
		}
	}
	return scores
}

// poolMaxNorms divides each score by the pool's best, the bm25 signal hybrid
// fuses.
func poolMaxNorms(scores []float64) []float64 {
	maxScore := 0.0
	for _, score := range scores {
		maxScore = max(maxScore, score)
	}
	norms := make([]float64, len(scores))
	if maxScore > 0 {
		for i, score := range scores {
			norms[i] = score / maxScore
		}
	}
	return norms
}

// gradeFromNorm maps hybrid's [0,1] fusion score to a grade.
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
