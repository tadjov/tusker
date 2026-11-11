package judgment

import (
	"context"
	"fmt"

	"github.com/tadjov/tusker/internal/bench/meta"
	"github.com/tadjov/tusker/internal/storage"
)

// Fusion weights for the hybrid score. Equal weighting is the common default
// for RRF-style hybrid retrieval; expose as constants so they're easy to tune.
const (
	hybridBM25Weight   = 0.5
	hybridVectorWeight = 0.5
)

const hybridRubricVersion = "v2"

// HybridStrategy fuses the BM25 lexical signal (pool-normalised) with vector
// cosine similarity (semantic signal), mirroring hybrid retrieval. The combined
// score is a weighted sum mapped to a grade 0-3. It judges the hybrid track on
// its own terms — neither pure keyword nor pure semantic relevance alone.
type HybridStrategy struct {
	vector *VectorStrategy
}

func NewHybridStrategy(opts StrategyOptions) (*HybridStrategy, error) {
	if opts.VectorStore == nil {
		return nil, errNoVectorStore(StrategyHybrid)
	}
	v, err := NewVectorStrategy(opts)
	if err != nil {
		return nil, err
	}
	return &HybridStrategy{vector: v}, nil
}

// NewHybridStrategyWithStore injects a vector store directly (used in tests).
func NewHybridStrategyWithStore(store storage.VectorStore, model string) *HybridStrategy {
	return &HybridStrategy{vector: NewVectorStrategyWithStore(store, model)}
}

func (HybridStrategy) Name() string { return string(StrategyHybrid) }

// Describe names both fusion components: BM25 contributes no model, so the
// embedding model is what distinguishes one hybrid grade set from another.
func (s HybridStrategy) Describe() meta.Judge {
	model := "embedding"
	if s.vector != nil && s.vector.model != "" {
		model = s.vector.model
	}
	return meta.Judge{Strategy: string(StrategyHybrid), Model: "bm25+" + model, RubricVersion: hybridRubricVersion}
}

func (s HybridStrategy) GradePool(ctx context.Context, q GradingQuery, docs []GradingDoc) ([]GradedDoc, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	bm25Terms, err := hybridQueryTerms(q)
	if err != nil {
		return nil, err
	}
	bm25Norms := hybridPoolMaxNorms(bm25Score(bm25Terms, analyzeDocs(docs)).docs)
	qVec, vecs, err := s.vector.vectors(ctx, q, docs)
	if err != nil {
		return nil, fmt.Errorf("hybrid vector component: %w", err)
	}

	// Docs without a stored embedding are omitted (recorded Unjudged by the
	// runner) rather than fused on the lexical signal alone.
	out := make([]GradedDoc, 0, len(docs))
	for i, d := range docs {
		dv, ok := vecs[d.ID]
		if !ok {
			continue
		}
		cos := cosine(qVec, dv)
		if cos < 0 {
			cos = 0 // cosine can be negative; clamp so it doesn't cancel BM25
		}
		combined := hybridBM25Weight*bm25Norms[i] + hybridVectorWeight*cos
		out = append(out, GradedDoc{DocID: d.ID, Grade: gradeFromHybridScore(combined)})
	}
	return out, nil
}

// hybridQueryTerms reads the whole text as terms, whatever the query's
// category: hybrid applies no phrase or boolean gate.
func hybridQueryTerms(q GradingQuery) ([]string, error) {
	out := scoringTerms(q.Text)
	if len(out) == 0 {
		return nil, &NoQueryTermsError{QueryID: q.ID}
	}
	return out, nil
}

// hybridPoolMaxNorms divides each bm25 score by the pool's best, the text
// signal hybrid fuses.
func hybridPoolMaxNorms(scores []float64) []float64 {
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

// gradeFromHybridScore maps hybrid's [0,1] fusion score to a grade.
func gradeFromHybridScore(norm float64) int {
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
