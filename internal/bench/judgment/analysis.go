package judgment

import (
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/kljensen/snowball/english"
	"github.com/tadjov/tusker/internal/bench/suite"
)

// englishStop is Postgres 18's tsearch_data/english.stop verbatim: Snowball's
// English list, which holds Lucene's _english_.
//
//go:embed english.stop
var englishStop string

// stopwords are never a term of a query or a document, and blank their
// position in the gate terms of both.
var stopwords = wordSet(englishStop)

func wordSet(list string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, w := range strings.Fields(list) {
		set[w] = struct{}{}
	}
	return set
}

// words splits s into its lowercase runs of letters and digits, in order.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// stem is Snowball English (Porter2).
func stem(word string) string { return english.Stem(word, true) }

// terms are the stems of s's words that are not stopwords, in order: the
// analysis every term judge applies to query and document alike. Skipping the
// stopword itself, not its stem, keeps "Downing" from matching "down".
func terms(s string) []string {
	var out []string
	for _, w := range words(s) {
		if _, stop := stopwords[w]; !stop {
			out = append(out, stem(w))
		}
	}
	return out
}

// gateTerms analyse s for the category gates as Postgres analyses a tsquery
// and a tsvector: each word keeps its position, and a stopword leaves a hole
// there.
func gateTerms(s string) []string {
	ws := words(s)
	for i, w := range ws {
		if _, stop := stopwords[w]; stop {
			ws[i] = ""
			continue
		}
		ws[i] = stem(w)
	}
	return ws
}

// gatePhrase is s's gate terms without the holes at either end, as
// phraseto_tsquery drops them. A hole inside it matches any one position.
func gatePhrase(s string) []string {
	phrase := gateTerms(s)
	for len(phrase) > 0 && phrase[0] == "" {
		phrase = phrase[1:]
	}
	for len(phrase) > 0 && phrase[len(phrase)-1] == "" {
		phrase = phrase[:len(phrase)-1]
	}
	return phrase
}

// analyzedDoc holds each field's terms in order, each field's gate terms, and
// the columns a query's filters read.
type analyzedDoc struct {
	title, description, content []string
	gateFields                  [][]string
	publishedAt                 time.Time
	publisherDomain             string
}

func analyzeDoc(d GradingDoc) analyzedDoc {
	return analyzedDoc{
		title:           terms(d.Title),
		description:     terms(d.Description),
		content:         terms(d.Content),
		gateFields:      [][]string{gateTerms(d.Title), gateTerms(d.Description), gateTerms(d.Content)},
		publishedAt:     d.PublishedAt,
		publisherDomain: d.PublisherDomain,
	}
}

func (d analyzedDoc) fields() [][]string { return [][]string{d.title, d.description, d.content} }

func (d analyzedDoc) holds(term string) bool {
	for _, field := range d.fields() {
		if slices.Contains(field, term) {
			return true
		}
	}
	return false
}

// holdsPhrase reports whether one field holds the phrase's gate terms at the
// phrase's positions; a hole in the phrase matches any term.
func (d analyzedDoc) holdsPhrase(phrase []string) bool {
	for _, field := range d.gateFields {
		for start := 0; start+len(phrase) <= len(field); start++ {
			if phraseAt(field[start:start+len(phrase)], phrase) {
				return true
			}
		}
	}
	return false
}

func phraseAt(window, phrase []string) bool {
	for i, term := range phrase {
		if term != "" && window[i] != term {
			return false
		}
	}
	return true
}

// NoQueryTermsError reports a query with no gradable terms. It is a property of
// the query, so every document fails the same way and no retry can fix it.
type NoQueryTermsError struct {
	QueryID string
}

func (e *NoQueryTermsError) Error() string {
	return fmt.Sprintf("query %q has no usable text (fix its text in the suite and re-pool, or set --strategy manual)", e.QueryID)
}

// BooleanTextError reports a boolean query whose text does not parse, which,
// like a query without terms, fails every document of the query.
type BooleanTextError struct {
	QueryID string
	Err     error
}

func (e *BooleanTextError) Error() string {
	return fmt.Sprintf("query %q: %v (fix its text in the suite and re-pool)", e.QueryID, e.Err)
}

func (e *BooleanTextError) Unwrap() error { return e.Err }

// FailsWholeQuery reports an error that is a property of one query, so that
// query's docs stay unjudged and the other queries are still graded.
func FailsWholeQuery(err error) bool {
	var noTerms *NoQueryTermsError
	var unparsed *BooleanTextError
	return errors.As(err, &noTerms) || errors.As(err, &unparsed)
}

// scoringTerms returns the texts' distinct terms, sorted.
func scoringTerms(texts ...string) []string {
	out := terms(strings.Join(texts, " "))
	slices.Sort(out)
	return slices.Compact(out)
}

// termQuery is a query as the keyword and bm25 judges read it: the terms that
// score a document, and the filters and category gate a document must pass to
// score at all.
type termQuery struct {
	// terms exclude a boolean query's negated operands.
	terms   []string
	filters suite.Filters
	// gate is nil when every document may score.
	gate docMatcher
}

func (q termQuery) admits(d analyzedDoc) bool {
	return passesFilters(q.filters, d) && (q.gate == nil || q.gate.matches(d))
}

// passesFilters fails an undated article under any date filter, as an engine's
// comparison against a NULL published_at does.
func passesFilters(f suite.Filters, d analyzedDoc) bool {
	if f.PublisherDomain != "" && d.publisherDomain != f.PublisherDomain {
		return false
	}
	dated := !d.publishedAt.IsZero()
	if !f.PublishedAfter.IsZero() && (!dated || d.publishedAt.Before(f.PublishedAfter)) {
		return false
	}
	if !f.PublishedBefore.IsZero() && (!dated || !d.publishedAt.Before(f.PublishedBefore)) {
		return false
	}
	return true
}

func readTermQuery(q GradingQuery) (termQuery, error) {
	var tq termQuery
	switch q.Category {
	case suite.CategoryPhrase:
		tq = termQuery{terms: scoringTerms(q.Text), gate: operandMatcher(q.Text)}
	case suite.CategoryBoolean:
		expr, err := suite.ParseBoolean(q.Text)
		if err != nil {
			return termQuery{}, &BooleanTextError{QueryID: q.ID, Err: err}
		}
		gate, positives := booleanGate(expr)
		tq = termQuery{terms: scoringTerms(positives...), gate: gate}
	default:
		tq = termQuery{terms: scoringTerms(q.Text)}
	}
	if len(tq.terms) == 0 {
		return termQuery{}, &NoQueryTermsError{QueryID: q.ID}
	}
	tq.filters = q.Filters
	return tq, nil
}

type docMatcher interface {
	matches(d analyzedDoc) bool
}

type termMatcher string

func (m termMatcher) matches(d analyzedDoc) bool { return d.holds(string(m)) }

type phraseMatcher []string

func (m phraseMatcher) matches(d analyzedDoc) bool { return d.holdsPhrase(m) }

type notMatcher struct{ operand docMatcher }

func (m notMatcher) matches(d analyzedDoc) bool { return !m.operand.matches(d) }

type allOf []docMatcher

func (m allOf) matches(d analyzedDoc) bool {
	for _, operand := range m {
		if !operand.matches(d) {
			return false
		}
	}
	return true
}

type anyOf []docMatcher

func (m anyOf) matches(d analyzedDoc) bool {
	for _, operand := range m {
		if operand.matches(d) {
			return true
		}
	}
	return false
}

// operandMatcher matches text's gate terms as one phrase, or nothing when it
// has none: such an operand drops out, as to_tsquery drops a stopword.
func operandMatcher(text string) docMatcher {
	switch phrase := gatePhrase(text); len(phrase) {
	case 0:
		return nil
	case 1:
		return termMatcher(phrase[0])
	default:
		return phraseMatcher(phrase)
	}
}

// booleanGate turns a parsed boolean text into the gate it sets and the
// operands that are not negated. An operand without gate terms drops out of
// the AND, OR or NOT holding it, and a group left empty drops out in turn.
func booleanGate(expr suite.BooleanExpr) (docMatcher, []string) {
	var positives []string
	var build func(e suite.BooleanExpr, negated bool) docMatcher
	build = func(e suite.BooleanExpr, negated bool) docMatcher {
		switch e.Kind {
		case suite.BooleanNot:
			if operand := build(e.Operands[0], !negated); operand != nil {
				return notMatcher{operand: operand}
			}
			return nil
		case suite.BooleanAnd, suite.BooleanOr:
			var operands []docMatcher
			for _, operand := range e.Operands {
				if m := build(operand, negated); m != nil {
					operands = append(operands, m)
				}
			}
			switch {
			case len(operands) == 0:
				return nil
			case len(operands) == 1:
				return operands[0]
			case e.Kind == suite.BooleanAnd:
				return allOf(operands)
			default:
				return anyOf(operands)
			}
		default:
			m := operandMatcher(e.Operand)
			if m != nil && !negated {
				positives = append(positives, e.Operand)
			}
			return m
		}
	}
	return build(expr, false), positives
}
