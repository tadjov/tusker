package judgment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStrategy_KnownNames(t *testing.T) {
	s, err := NewStrategy(StrategyKeyword, StrategyOptions{})
	require.NoError(t, err)
	assert.Equal(t, "keyword", s.Name())

	s, err = NewStrategy(StrategyManual, StrategyOptions{})
	require.NoError(t, err)
	assert.Equal(t, "manual", s.Name())

	s, err = NewStrategy(StrategyBM25, StrategyOptions{})
	require.NoError(t, err)
	assert.Equal(t, "bm25", s.Name())
}

func TestNewStrategy_VectorHybridRequireVectorStore(t *testing.T) {
	// vector/hybrid are implemented but need a vector store; without one they
	// must fail loudly rather than silently producing no grades.
	for _, kind := range []StrategyKind{StrategyVector, StrategyHybrid} {
		_, err := NewStrategy(kind, StrategyOptions{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "vector store",
			"strategy %q should require a vector store", kind)
	}
}

func TestNewStrategy_VectorHybridWithStore(t *testing.T) {
	for _, kind := range []StrategyKind{StrategyVector, StrategyHybrid} {
		s, err := NewStrategy(kind, StrategyOptions{VectorStore: fakeVectorStore{}})
		require.NoError(t, err)
		assert.Equal(t, string(kind), s.Name())
	}
}

func TestNewStrategy_UnknownName(t *testing.T) {
	_, err := NewStrategy("bogus", StrategyOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown strategy")
}

// Heuristic judges have no prompt, so the rubric version is what tells two
// grade sets under the same strategy apart.
func TestHeuristicStrategies_NameTheirRubric(t *testing.T) {
	for _, kind := range []StrategyKind{StrategyKeyword, StrategyBM25, StrategyVector, StrategyHybrid} {
		s, err := NewStrategy(kind, StrategyOptions{VectorStore: fakeVectorStore{}})
		require.NoError(t, err)
		assert.NotEmpty(t, s.Describe().RubricVersion, "strategy %q", kind)
	}
}
