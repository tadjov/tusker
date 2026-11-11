package judgment

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// termJudges grade one query's docs through each term judge's public
// interface, keyed by doc title.
var termJudges = map[string]func(t *testing.T, q GradingQuery, docs []GradingDoc) map[string]int{
	"keyword": func(t *testing.T, q GradingQuery, docs []GradingDoc) map[string]int {
		t.Helper()
		out := map[string]int{}
		for _, d := range docs {
			g, err := NewKeywordStrategy().Grade(context.Background(), q, d)
			require.NoError(t, err)
			out[d.Title] = g
		}
		return out
	},
	"bm25": func(t *testing.T, q GradingQuery, docs []GradingDoc) map[string]int {
		t.Helper()
		graded, err := NewBM25Strategy().GradePool(context.Background(), q, docs)
		require.NoError(t, err)
		title := map[uuid.UUID]string{}
		for _, d := range docs {
			title[d.ID] = d.Title
		}
		out := map[string]int{}
		for _, g := range graded {
			out[title[g.DocID]] = g.Grade
		}
		return out
	},
}

func titled(titles ...string) []GradingDoc {
	docs := make([]GradingDoc, len(titles))
	for i, title := range titles {
		docs[i] = GradingDoc{ID: uuid.New(), Title: title}
	}
	return docs
}

func TestTermJudges_GatePhraseAndBooleanQueries(t *testing.T) {
	tests := []struct {
		name     string
		query    GradingQuery
		docs     []GradingDoc
		zero     []string
		positive []string
	}{
		{
			name:     "a NOT violation grades 0 and compliance does not",
			query:    GradingQuery{ID: "q", Text: "climate AND NOT politics", Category: "boolean"},
			docs:     titled("Climate politics heats up", "Climate report published", "Football results"),
			zero:     []string{"Climate politics heats up", "Football results"},
			positive: []string{"Climate report published"},
		},
		{
			name:     "a scrambled phrase grades 0",
			query:    GradingQuery{ID: "q", Text: "global warming", Category: "phrase"},
			docs:     titled("Warming global markets", "Global warming accelerates", "Weather today"),
			zero:     []string{"Warming global markets", "Weather today"},
			positive: []string{"Global warming accelerates"},
		},
		{
			name:  "a phrase must sit in one field",
			query: GradingQuery{ID: "q", Text: "global warming", Category: "phrase"},
			docs: []GradingDoc{
				{ID: uuid.New(), Title: "A global summit", Description: "Warming oceans"},
				{ID: uuid.New(), Title: "Summit", Description: "Global warming talks"},
			},
			zero:     []string{"A global summit"},
			positive: []string{"Summit"},
		},
		{
			name:     "a phrase matches its inflected form",
			query:    GradingQuery{ID: "q", Text: "trade wars", Category: "phrase"},
			docs:     titled("The trade war escalates", "War on trade barriers"),
			zero:     []string{"War on trade barriers"},
			positive: []string{"The trade war escalates"},
		},
		{
			name:  "OR and parentheses",
			query: GradingQuery{ID: "q", Text: "(election OR referendum) AND NOT football", Category: "boolean"},
			docs: append([]GradingDoc{
				{ID: uuid.New(), Title: "Referendum", Content: "The referendum on the referendum law"},
				{ID: uuid.New(), Title: "Election", Content: "Election day and the election count"},
			}, titled("Football referendum", "Weather report")...),
			zero:     []string{"Football referendum", "Weather report"},
			positive: []string{"Referendum", "Election"},
		},
		{
			name:  "NOT binds tighter than AND, and AND tighter than OR",
			query: GradingQuery{ID: "q", Text: "climate OR energy AND NOT politics", Category: "boolean"},
			docs: append(titled("Climate politics", "Energy politics"),
				GradingDoc{ID: uuid.New(), Title: "Energy prices", Content: "Energy costs and energy bills"}),
			zero:     []string{"Energy politics"},
			positive: []string{"Climate politics", "Energy prices"},
		},
		{
			name:     "adjacent operands are ANDed",
			query:    GradingQuery{ID: "q", Text: "climate summit NOT politics", Category: "boolean"},
			docs:     titled("Climate summit opens", "Climate report", "Climate summit politics"),
			zero:     []string{"Climate report", "Climate summit politics"},
			positive: []string{"Climate summit opens"},
		},
		{
			name:     "a quoted operand is a phrase",
			query:    GradingQuery{ID: "q", Text: `"trade war" AND NOT china`, Category: "boolean"},
			docs:     titled("Trade war with Europe", "War over trade", "China trade war"),
			zero:     []string{"War over trade", "China trade war"},
			positive: []string{"Trade war with Europe"},
		},
	}
	for judge, grade := range termJudges {
		for _, tt := range tests {
			t.Run(judge+"/"+tt.name, func(t *testing.T) {
				got := grade(t, tt.query, tt.docs)

				for _, title := range tt.zero {
					assert.Equal(t, GradeNotRelev, got[title], "%q", title)
				}
				for _, title := range tt.positive {
					assert.Greater(t, got[title], GradeNotRelev, "%q", title)
				}
			})
		}
	}
}

// The gate decides which docs may score; it does not change how a doc that
// passes scores, so bm25's statistics still cover the whole pool.
func TestBM25GradePool_GateLeavesPassingDocsGradesUnchanged(t *testing.T) {
	docs := []GradingDoc{
		{ID: uuid.New(), Title: "Global warming accelerates", Content: "Global warming is speeding up."},
		{ID: uuid.New(), Title: "Warming of global markets", Content: "Markets warming up globally."},
		{ID: uuid.New(), Title: "Global warming", Content: "A report on global trade and warming."},
		{ID: uuid.New(), Title: "Weather today", Content: "Sunny."},
	}
	gated := termJudges["bm25"](t, GradingQuery{ID: "q", Text: "global warming", Category: "phrase"}, docs)
	ungated := termJudges["bm25"](t, GradingQuery{ID: "q", Text: "global warming", Category: "keyword"}, docs)

	assert.Equal(t, GradeNotRelev, gated["Warming of global markets"])
	assert.Greater(t, ungated["Warming of global markets"], GradeNotRelev)
	for _, passing := range []string{"Global warming accelerates", "Global warming"} {
		assert.Equal(t, ungated[passing], gated[passing], "%q", passing)
	}
}

func TestTermJudges_RefuseABooleanTextThatDoesNotParse(t *testing.T) {
	for _, text := range []string{"climate AND (politics", "climate AND", "OR climate"} {
		q := GradingQuery{ID: "q1", Text: text, Category: "boolean"}
		doc := GradingDoc{ID: uuid.New(), Title: "Climate politics"}

		_, keywordErr := NewKeywordStrategy().Grade(context.Background(), q, doc)
		_, bm25Err := NewBM25Strategy().GradePool(context.Background(), q, []GradingDoc{doc})

		for judge, err := range map[string]error{"keyword": keywordErr, "bm25": bm25Err} {
			var noTerms *NoQueryTermsError
			if !errors.As(err, &noTerms) || noTerms.QueryID != "q1" {
				t.Errorf("%s on %q: err = %v, want a NoQueryTermsError for q1", judge, text, err)
			}
		}
	}
}

func TestTermJudges_GradeTheQueryTextNeverTheDescription(t *testing.T) {
	perfect := GradingDoc{
		ID:          uuid.New(),
		Title:       "Climate change is accelerating",
		Description: "Scientists warn climate change is speeding up",
		Content:     "Climate change reshapes weather, and climate change drives migration.",
	}
	docs := append([]GradingDoc{perfect}, titled("Local football results", "Markets close higher", "Weather today")...)
	const description = "Simple keyword search for climate change articles"

	for judge, grade := range termJudges {
		t.Run(judge, func(t *testing.T) {
			got := grade(t, GradingQuery{ID: "q1", Text: "climate change", Description: description}, docs)

			assert.Equal(t, GradeHighly, got[perfect.Title])
		})
	}

	described := GradingQuery{ID: "q1", Description: description}
	_, keywordErr := NewKeywordStrategy().Grade(context.Background(), described, perfect)
	_, bm25Err := NewBM25Strategy().GradePool(context.Background(), described, docs)
	for judge, err := range map[string]error{"keyword": keywordErr, "bm25": bm25Err} {
		var noTerms *NoQueryTermsError
		assert.True(t, errors.As(err, &noTerms), "%s graded a query with only a description: err = %v", judge, err)
	}
}
