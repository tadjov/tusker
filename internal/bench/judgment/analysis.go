package judgment

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kljensen/snowball/english"
)

// minTermRunes keeps two-letter terms such as "US" and "EU".
const minTermRunes = 2

// stopwords are dropped from a query's terms, never from a document's.
var stopwords = map[string]struct{}{
	"an": {}, "and": {}, "the": {}, "or": {}, "of": {}, "to": {},
	"in": {}, "on": {}, "at": {}, "by": {}, "for": {}, "with": {}, "from": {},
	"is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "been": {}, "being": {},
	"it": {}, "its": {}, "as": {}, "that": {}, "this": {}, "these": {}, "those": {},
}

// words splits s into its lowercase runs of letters and digits, in order.
func words(s string) []string {
	split := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	kept := split[:0]
	for _, w := range split {
		if utf8.RuneCountInString(w) >= minTermRunes {
			kept = append(kept, w)
		}
	}
	return kept
}

// stem is Snowball English, the stemmer every engine analyses English with.
func stem(word string) string { return english.Stem(word, true) }

// terms are the stems of s's words, in order: the analysis every term judge
// applies to query and document alike.
func terms(s string) []string {
	ws := words(s)
	for i, w := range ws {
		ws[i] = stem(w)
	}
	return ws
}

func termSet(s string) map[string]bool {
	out := make(map[string]bool)
	for _, t := range terms(s) {
		out[t] = true
	}
	return out
}

func termCounts(s string) map[string]int {
	out := make(map[string]int)
	for _, t := range terms(s) {
		out[t]++
	}
	return out
}

// NoQueryTermsError reports a query with no gradable terms. It is a property of
// the query, so every document fails the same way and no retry can fix it.
type NoQueryTermsError struct {
	QueryID string
}

func (e *NoQueryTermsError) Error() string {
	return fmt.Sprintf("query %q has no usable text (fix its text in the suite and re-pool, or set --strategy manual)", e.QueryID)
}

// queryTerms returns the distinct stems of the query text's words that are not
// stopwords, sorted.
func queryTerms(q GradingQuery) ([]string, error) {
	var out []string
	for _, w := range words(q.Text) {
		if _, skip := stopwords[w]; skip {
			continue
		}
		out = append(out, stem(w))
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) == 0 {
		return nil, &NoQueryTermsError{QueryID: q.ID}
	}
	return out, nil
}
