package judgment

import (
	"context"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/bench/meta"
	"github.com/tadjov/tusker/internal/storage"
)

// VectorStrategy grades candidates by cosine similarity between the query
// embedding and each document's stored embedding. Unlike keyword/bm25 it
// captures semantic relevance — paraphrases and related concepts with no shared
// keywords — which is what the semantic and hybrid tracks need.
//
// It is storage-agnostic: document vectors come from a storage.VectorStore
// (Postgres today, ES later) and only the query is embedded at runtime. It does
// not re-embed documents.
type VectorStrategy struct {
	store storage.VectorStore
	model string
}

// NewVectorStrategy builds the strategy from options. A VectorStore must be
// supplied by the caller (cmd_judge wires it from the PG pool + embedder).
func NewVectorStrategy(opts StrategyOptions) (*VectorStrategy, error) {
	if opts.VectorStore == nil {
		return nil, errNoVectorStore(StrategyVector)
	}
	return &VectorStrategy{store: opts.VectorStore, model: opts.EmbeddingModel}, nil
}

// NewVectorStrategyWithStore injects a store directly (used in tests).
func NewVectorStrategyWithStore(store storage.VectorStore, model string) *VectorStrategy {
	return &VectorStrategy{store: store, model: model}
}

func (VectorStrategy) Name() string { return string(StrategyVector) }

// Describe records the embedding model that produced the cosine scores, so a
// vector-graded qrel set names the model it depends on.
func (s VectorStrategy) Describe() meta.Judge {
	return meta.Judge{Strategy: string(StrategyVector), Model: s.model, RubricVersion: vectorRubricVersion}
}

const vectorRubricVersion = "v1"

// vectorFetchBatchSize fetches a typical query pool's vectors in one store round-trip.
const vectorFetchBatchSize = 512

func (VectorStrategy) PreferredBatchSize() int { return vectorFetchBatchSize }

func (s VectorStrategy) GradeBatch(ctx context.Context, q GradingQuery, docs []GradingDoc) ([]GradedDoc, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	qVec, vecs, err := s.vectors(ctx, q, docs)
	if err != nil {
		return nil, err
	}
	// Return grades only for docs that have a stored embedding. Docs without one
	// are simply omitted; the runner records them as Unjudged, leaving them for
	// another strategy rather than scoring them 0.
	out := make([]GradedDoc, 0, len(docs))
	for _, d := range docs {
		if dv, ok := vecs[d.ID]; ok {
			out = append(out, GradedDoc{DocID: d.ID, Grade: gradeFromCosine(cosine(qVec, dv))})
		}
	}
	return out, nil
}

func (s VectorStrategy) Grade(ctx context.Context, q GradingQuery, doc GradingDoc) (int, error) {
	qVec, vecs, err := s.vectors(ctx, q, []GradingDoc{doc})
	if err != nil {
		return GradeUnjudged, err
	}
	dv, ok := vecs[doc.ID]
	if !ok {
		return GradeUnjudged, fmt.Errorf("vector strategy: no stored embedding for doc %s", doc.ID)
	}
	return gradeFromCosine(cosine(qVec, dv)), nil
}

// vectors embeds the query once and fetches all candidate doc vectors from the
// store in one round-trip.
func (s VectorStrategy) vectors(ctx context.Context, q GradingQuery, docs []GradingDoc) ([]float32, map[uuid.UUID][]float32, error) {
	if s.store == nil {
		return nil, nil, fmt.Errorf("vector strategy: no vector store configured")
	}
	qVec, err := s.store.QueryVector(ctx, q.Text)
	if err != nil {
		return nil, nil, fmt.Errorf("embed query %q: %w", q.ID, err)
	}
	ids := make([]uuid.UUID, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}
	vecs, err := s.store.DocVectors(ctx, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch doc vectors: %w", err)
	}
	if err := checkVectorWidths(q.ID, qVec, docs, vecs); err != nil {
		return nil, nil, err
	}
	return qVec, vecs, nil
}

// VectorWidthError reports a query vector and a stored document vector of
// different widths. Cosine over them is meaningless, and because the widths
// come from the models rather than the document, every remaining document
// fails the same way — so the judge runner stops on this instead of falling
// back per-doc and writing an annotations file in which nothing is relevant.
type VectorWidthError struct {
	QueryID    string
	DocID      uuid.UUID
	QueryWidth int
	DocWidth   int
}

func (e *VectorWidthError) Error() string {
	if e.QueryWidth == 0 {
		return fmt.Sprintf("query %q embedded to an empty vector: the embedding endpoint returned no dimensions", e.QueryID)
	}
	return fmt.Sprintf(
		"query %q embeds to %d dimensions but document %s is stored with %d: the query embedder and the stored vectors are different models",
		e.QueryID, e.QueryWidth, e.DocID, e.DocWidth)
}

// checkVectorWidths walks the documents in pool order so the error names the
// same document on every run.
func checkVectorWidths(queryID string, qVec []float32, docs []GradingDoc, vecs map[uuid.UUID][]float32) error {
	if len(qVec) == 0 {
		return &VectorWidthError{QueryID: queryID}
	}
	for _, d := range docs {
		dv, ok := vecs[d.ID]
		if !ok {
			continue
		}
		if len(dv) != len(qVec) {
			return &VectorWidthError{QueryID: queryID, DocID: d.ID, QueryWidth: len(qVec), DocWidth: len(dv)}
		}
	}
	return nil
}

func errNoVectorStore(kind StrategyKind) error {
	return fmt.Errorf(
		"%s strategy requires a vector store: set --pg/PG_CONNECTION_STRING and an embedding endpoint (--embedding-base / EMBEDDING_BASE_URL)",
		kind)
}

func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// gradeFromCosine maps cosine similarity to a grade. Thresholds suit
// sentence-embedding models (e.g. qwen3-embedding) and may need tuning per
// model — they are intentionally conservative.
func gradeFromCosine(c float64) int {
	switch {
	case c >= 0.70:
		return GradeHighly
	case c >= 0.55:
		return GradeRelevant
	case c >= 0.40:
		return GradeMarginally
	default:
		return GradeNotRelev
	}
}
