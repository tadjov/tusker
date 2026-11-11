package judgment

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/tadjov/tusker/internal/bench/pool"
	"github.com/tadjov/tusker/internal/bench/suite"
	"github.com/tadjov/tusker/internal/types/document"
)

func TestTermJudges_GradeADocViolatingADeclaredFilterZero(t *testing.T) {
	march := time.Date(2024, 3, 10, 9, 0, 0, 0, time.UTC)
	article := func(title, domain string, published time.Time) GradingDoc {
		return GradingDoc{ID: uuid.New(), Title: title, PublisherDomain: domain, PublishedAt: published}
	}
	tests := []struct {
		name     string
		filters  suite.Filters
		docs     []GradingDoc
		zero     []string
		positive []string
	}{
		{
			name:     "another publisher",
			filters:  suite.Filters{PublisherDomain: "reuters.com"},
			docs:     []GradingDoc{article("Climate summit at Reuters", "reuters.com", march), article("Climate summit at the BBC", "bbc.co.uk", march)},
			zero:     []string{"Climate summit at the BBC"},
			positive: []string{"Climate summit at Reuters"},
		},
		{
			name:    "published on or before published_after",
			filters: suite.Filters{PublishedAfter: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)},
			docs: []GradingDoc{
				article("Climate summit in March", "reuters.com", march),
				article("Climate summit in February", "reuters.com", time.Date(2024, 2, 20, 0, 0, 0, 0, time.UTC)),
				article("Climate summit at midnight", "reuters.com", time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)),
			},
			zero:     []string{"Climate summit in February", "Climate summit at midnight"},
			positive: []string{"Climate summit in March"},
		},
		{
			name:    "published on or after published_before",
			filters: suite.Filters{PublishedBefore: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)},
			docs: []GradingDoc{
				article("Climate summit in March", "reuters.com", march),
				article("Climate summit in February", "reuters.com", time.Date(2024, 2, 20, 0, 0, 0, 0, time.UTC)),
			},
			zero:     []string{"Climate summit in March"},
			positive: []string{"Climate summit in February"},
		},
		{
			name:    "no publication date under a date filter",
			filters: suite.Filters{PublishedBefore: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)},
			docs: []GradingDoc{
				article("Climate summit undated", "reuters.com", time.Time{}),
				article("Climate summit in February", "reuters.com", time.Date(2024, 2, 20, 0, 0, 0, 0, time.UTC)),
			},
			zero:     []string{"Climate summit undated"},
			positive: []string{"Climate summit in February"},
		},
	}
	for judge, grade := range termJudges {
		for _, tt := range tests {
			t.Run(judge+"/"+tt.name, func(t *testing.T) {
				got := grade(t, GradingQuery{ID: "q", Text: "climate summit", Filters: tt.filters}, tt.docs)

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

// The judge runner hands each strategy the pool entry's filters and the
// article's publisher and date.
func TestRunner_FiltersReachTheJudge(t *testing.T) {
	reuters, bbc := uuid.New(), uuid.New()
	march := time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC)
	reader := &stubStorageReader{articles: map[uuid.UUID]document.Article{
		reuters: {ID: reuters, Title: "Climate summit", PublisherDomain: "reuters.com", PublishedAt: march},
		bbc:     {ID: bbc, Title: "Climate summit", PublisherDomain: "bbc.co.uk", PublishedAt: march},
	}}
	pf := &pool.PoolFile{Queries: []pool.PoolEntry{{
		QueryID:   "q1",
		QueryText: "climate summit",
		Filters:   suite.Filters{PublisherDomain: "reuters.com", PublishedAfter: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)},
		Docs:      []pool.PooledDoc{{DocID: reuters}, {DocID: bbc}},
	}}}

	for _, strategy := range []Strategy{NewKeywordStrategy(), NewBM25Strategy()} {
		t.Run(strategy.Name(), func(t *testing.T) {
			grades := runGrades(t, RunnerConfig{Strategy: strategy, Reader: reader}, pf)

			assert.Greater(t, grades[reuters], GradeNotRelev)
			assert.Equal(t, GradeNotRelev, grades[bbc])
		})
	}
}
