package judgment

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestKeywordStrategy_Grade(t *testing.T) {
	s := NewKeywordStrategy()
	ctx := context.Background()
	q := GradingQuery{ID: "q1", Description: "climate change global warming"}

	t.Run("title-dominant match grades highly", func(t *testing.T) {
		g, err := s.Grade(ctx, q, GradingDoc{
			ID:    uuid.New(),
			Title: "Climate change accelerates global warming",
		})
		assert.NoError(t, err)
		assert.Equal(t, GradeHighly, g)
	})

	t.Run("body-only match grades relevant", func(t *testing.T) {
		g, err := s.Grade(ctx, q, GradingDoc{
			ID:          uuid.New(),
			Title:       "Energy market update",
			Description: "Effects of climate change on global warming patterns",
		})
		assert.NoError(t, err)
		assert.Equal(t, GradeRelevant, g)
	})

	t.Run("partial body match grades marginal", func(t *testing.T) {
		g, err := s.Grade(ctx, q, GradingDoc{
			ID:      uuid.New(),
			Title:   "Travel news",
			Content: "tourism affected by climate",
		})
		assert.NoError(t, err)
		assert.Equal(t, GradeMarginally, g)
	})

	t.Run("no overlap grades zero", func(t *testing.T) {
		g, err := s.Grade(ctx, q, GradingDoc{
			ID:    uuid.New(),
			Title: "Local sports roundup",
		})
		assert.NoError(t, err)
		assert.Equal(t, GradeNotRelev, g)
	})

	t.Run("empty query errors so runner records as unjudged", func(t *testing.T) {
		_, err := s.Grade(ctx, GradingQuery{}, GradingDoc{Title: "anything"})
		assert.ErrorContains(t, err, "no usable description")
	})
}

func TestParseGradeJSON(t *testing.T) {
	id := "493384af-c5fc-4677-ad11-2081e73f0588"

	t.Run("plain JSON", func(t *testing.T) {
		g, err := ParseGradeJSON(`{"doc_id":"`+id+`","grade":2}`, id)
		assert.NoError(t, err)
		assert.Equal(t, 2, g)
	})

	t.Run("JSON with surrounding prose", func(t *testing.T) {
		raw := "Here is my grade:\n{\"doc_id\":\"" + id + "\",\"grade\":3}\nThanks!"
		g, err := ParseGradeJSON(raw, id)
		assert.NoError(t, err)
		assert.Equal(t, 3, g)
	})

	t.Run("doc_id mismatch errors", func(t *testing.T) {
		other := "00000000-0000-0000-0000-000000000000"
		_, err := ParseGradeJSON(`{"doc_id":"`+other+`","grade":2}`, id)
		assert.ErrorContains(t, err, "doc_id mismatch")
	})

	t.Run("out-of-range grade errors", func(t *testing.T) {
		_, err := ParseGradeJSON(`{"doc_id":"`+id+`","grade":7}`, id)
		assert.ErrorContains(t, err, "out of range")
	})
}
