package judgment

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/apperr"
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
				{ID: uuid.New(), Title: "Markets go global", Description: "Warming oceans worry insurers"},
				{ID: uuid.New(), Title: "Summit", Description: "Global warming talks"},
			},
			zero:     []string{"A global summit", "Markets go global"},
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
			name:     "NOT negates only its own operand",
			query:    GradingQuery{ID: "q", Text: "NOT politics climate", Category: "boolean"},
			docs:     titled("Climate report", "Climate politics", "Football results"),
			zero:     []string{"Climate politics", "Football results"},
			positive: []string{"Climate report"},
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
		{
			name:     "a stopword opening a phrase leaves it, as phraseto_tsquery builds 'white' <-> 'hous'",
			query:    GradingQuery{ID: "q", Text: "the White House", Category: "phrase"},
			docs:     titled("White House officials said", "House of white paint"),
			zero:     []string{"House of white paint"},
			positive: []string{"White House officials said"},
		},
		{
			name:     "a stopword in the document keeps its position, as 'trade' <-> 'war' matches it",
			query:    GradingQuery{ID: "q", Text: "the trade war", Category: "phrase"},
			docs:     titled("A trade war looms", "War on trade"),
			zero:     []string{"War on trade"},
			positive: []string{"A trade war looms"},
		},
		{
			name:     "a stopword inside a phrase keeps its position, as 'war' <2> 'word'",
			query:    GradingQuery{ID: "q", Text: "war of words", Category: "phrase"},
			docs:     titled("A war of words erupts", "Words of war", "War words", "War of the words"),
			zero:     []string{"Words of war", "War words", "War of the words"},
			positive: []string{"A war of words erupts"},
		},
		{
			name:     "every word Postgres stops holds a position, as 'climat' <-> 'chang' <3> 'stop'",
			query:    GradingQuery{ID: "q", Text: "climate change will not stop", Category: "phrase"},
			docs:     titled("Climate change can not stop", "Climate change stop", "Stop climate change"),
			zero:     []string{"Climate change stop", "Stop climate change"},
			positive: []string{"Climate change can not stop"},
		},
		{
			name:     "a phrase opening with stopwords keeps only its last word, as phraseto_tsquery builds 'storm'",
			query:    GradingQuery{ID: "q", Text: "after the storm", Category: "phrase"},
			docs:     titled("Before the storm hits", "Calm weather"),
			zero:     []string{"Calm weather"},
			positive: []string{"Before the storm hits"},
		},
		{
			name:     "a quoted operand loses its stopwords, as to_tsquery builds 'brexit' & 'deal'",
			query:    GradingQuery{ID: "q", Text: `Brexit AND "no deal"`, Category: "boolean"},
			docs:     titled("Brexit deal agreed", "Brexit talks stall"),
			zero:     []string{"Brexit talks stall"},
			positive: []string{"Brexit deal agreed"},
		},
		{
			name:     "a stem equal to a stopword never matches that stopword",
			query:    GradingQuery{ID: "q", Text: "Downing AND Johnson", Category: "boolean"},
			docs:     titled("Prices went down, Johnson said", "Downing Street: Johnson said"),
			zero:     []string{"Prices went down, Johnson said"},
			positive: []string{"Downing Street: Johnson said"},
		},
		{
			name:     "a one-word phrase stemming to a stopword never matches that stopword",
			query:    GradingQuery{ID: "q", Text: "others", Category: "phrase"},
			docs:     titled("The other day", "Others agree"),
			zero:     []string{"The other day"},
			positive: []string{"Others agree"},
		},
		{
			name:     "a phrase stemming to a stopword needs the word that is not one",
			query:    GradingQuery{ID: "q", Text: "others say", Category: "phrase"},
			docs:     titled("Other say", "Others say"),
			zero:     []string{"Other say"},
			positive: []string{"Others say"},
		},
		{
			name:     "a stopword operand drops out of AND, as to_tsquery drops it",
			query:    GradingQuery{ID: "q", Text: "climate AND the", Category: "boolean"},
			docs:     titled("Climate report", "Football results"),
			zero:     []string{"Football results"},
			positive: []string{"Climate report"},
		},
		{
			name:     "a stopword operand drops out of NOT",
			query:    GradingQuery{ID: "q", Text: "climate NOT it", Category: "boolean"},
			docs:     titled("It is climate", "Football results"),
			zero:     []string{"Football results"},
			positive: []string{"It is climate"},
		},
		{
			name:     "a one-letter word is a term, as to_tsquery builds 'vitamin' & 'd' & 'defici'",
			query:    GradingQuery{ID: "q", Text: "vitamin D AND deficiency", Category: "boolean"},
			docs:     titled("Vitamin D deficiency rises", "Vitamin deficiency", "Vitamin pills sell"),
			zero:     []string{"Vitamin deficiency", "Vitamin pills sell"},
			positive: []string{"Vitamin D deficiency rises"},
		},
		{
			name:     "an abbreviation keeps its letters that are not stopwords, so U.S. needs u and not s",
			query:    GradingQuery{ID: "q", Text: "U.S. AND election", Category: "boolean"},
			docs:     titled("U.S. election results", "U election results", "UK election results", "Football results"),
			zero:     []string{"UK election results", "Football results"},
			positive: []string{"U.S. election results", "U election results"},
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

// The gated-out docs hold "global" but not "warming", so they count toward
// N and df(global). Every doc is three terms long, so avgdl = 3 either way.
// Over the whole pool, N = 6, df(global) = 5 and df(warm) = 1, so
// idf(global) = ln(1 + 1.5/5.5) = 0.241 and idf(warm) = ln(1 + 5.5/1.5) = 1.540;
// the admitted doc holds global once and warming twice at average length, so
// its norm is (0.241·1 + 1.540·2·2.5/3.5) / (2.5·1.781) = 0.548, grade 3. Over
// the admitted doc and the empty one alone, both idfs are ln 2 and the norm
// is (1 + 1.429) / 5 = 0.486, grade 2.
func TestBM25GradePool_GatedOutDocsStillCountInTheTermStatistics(t *testing.T) {
	admitted := GradingDoc{ID: uuid.New(), Title: "Global warming", Content: "warming"}
	docs := append([]GradingDoc{admitted},
		titled("Global markets rally", "Global stocks rally", "Global bonds rally", "Global trade rally", "Weather report today")...)

	got := termJudges["bm25"](t, GradingQuery{ID: "q", Text: "global warming", Category: "phrase"}, docs)

	assert.Equal(t, GradeHighly, got["Global warming"])
	for _, gated := range []string{"Global markets rally", "Global stocks rally", "Global bonds rally", "Global trade rally"} {
		assert.Equal(t, GradeNotRelev, got[gated], "%q", gated)
	}
}

func TestTermJudges_RefuseABooleanTextThatDoesNotParse(t *testing.T) {
	for _, text := range []string{"climate AND (politics", "climate AND", "OR climate"} {
		q := GradingQuery{ID: "q1", Text: text, Category: "boolean"}
		doc := GradingDoc{ID: uuid.New(), Title: "Climate politics"}

		_, keywordErr := NewKeywordStrategy().Grade(context.Background(), q, doc)
		_, bm25Err := NewBM25Strategy().GradePool(context.Background(), q, []GradingDoc{doc})

		for judge, err := range map[string]error{"keyword": keywordErr, "bm25": bm25Err} {
			var unparsed *BooleanTextError
			if !errors.As(err, &unparsed) || unparsed.QueryID != "q1" {
				t.Errorf("%s on %q: err = %v, want a BooleanTextError for q1", judge, text, err)
			}
		}
	}
}

// phraseto_tsquery('english', 'will not') is empty, so the phrase has nothing
// to grade.
func TestTermJudges_RefuseAPhraseOfStopwordsOnly(t *testing.T) {
	q := GradingQuery{ID: "q1", Text: "will not", Category: "phrase"}
	doc := GradingDoc{ID: uuid.New(), Title: "It will not"}

	_, keywordErr := NewKeywordStrategy().Grade(context.Background(), q, doc)
	_, bm25Err := NewBM25Strategy().GradePool(context.Background(), q, []GradingDoc{doc})

	for judge, err := range map[string]error{"keyword": keywordErr, "bm25": bm25Err} {
		var noTerms *NoQueryTermsError
		if !errors.As(err, &noTerms) || noTerms.QueryID != "q1" {
			t.Errorf("%s: err = %v, want a NoQueryTermsError for q1", judge, err)
		}
	}
}

func TestTermJudges_KeepTheTokenizersValidationErrorInTheChain(t *testing.T) {
	q := GradingQuery{ID: "q1", Text: "climate AND (politics", Category: "boolean"}

	_, err := NewKeywordStrategy().Grade(context.Background(), q, GradingDoc{ID: uuid.New(), Title: "Climate"})

	var validation *apperr.ValidationError
	assert.ErrorAs(t, err, &validation)
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
