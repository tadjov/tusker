package judgment

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kljensen/snowball/english"
	"github.com/tadjov/tusker/internal/bench/suite"
	"github.com/tadjov/tusker/internal/token"
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

// analyzedDoc holds each field's terms in order, and the columns a query's
// filters read.
type analyzedDoc struct {
	title, description, content []string
	publishedAt                 time.Time
	publisherDomain             string
}

func analyzeDoc(d GradingDoc) analyzedDoc {
	return analyzedDoc{
		title:           terms(d.Title),
		description:     terms(d.Description),
		content:         terms(d.Content),
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

// holdsPhrase reports whether one field holds the phrase's terms adjacent and
// in order.
func (d analyzedDoc) holdsPhrase(phrase []string) bool {
	for _, field := range d.fields() {
		for start := 0; start+len(phrase) <= len(field); start++ {
			if slices.Equal(field[start:start+len(phrase)], phrase) {
				return true
			}
		}
	}
	return false
}

// NoQueryTermsError reports a query with no gradable terms. It is a property of
// the query, so every document fails the same way and no retry can fix it.
type NoQueryTermsError struct {
	QueryID string
	// Reason is why the text yields no terms; empty when it simply has none.
	Reason string
}

func (e *NoQueryTermsError) Error() string {
	reason := ""
	if e.Reason != "" {
		reason = ": " + e.Reason
	}
	return fmt.Sprintf("query %q has no usable text%s (fix its text in the suite and re-pool, or set --strategy manual)", e.QueryID, reason)
}

// scoringTerms returns the distinct stems of the texts' words that are not
// stopwords, sorted.
func scoringTerms(texts ...string) []string {
	var out []string
	for _, text := range texts {
		for _, w := range words(text) {
			if _, skip := stopwords[w]; !skip {
				out = append(out, stem(w))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// queryTerms reads the whole text as terms, whatever the query's category.
func queryTerms(q GradingQuery) ([]string, error) {
	out := scoringTerms(q.Text)
	if len(out) == 0 {
		return nil, &NoQueryTermsError{QueryID: q.ID}
	}
	return out, nil
}

// Categories whose queries constrain which documents can be relevant at all.
const (
	categoryPhrase  = "phrase"
	categoryBoolean = "boolean"
)

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
	if !f.PublishedAfter.IsZero() && (!dated || !d.publishedAt.After(f.PublishedAfter)) {
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
	case categoryPhrase:
		tq = termQuery{terms: scoringTerms(q.Text), gate: phraseMatcher(terms(q.Text))}
	case categoryBoolean:
		gate, positives, err := parseBoolean(q.Text)
		if err != nil {
			return termQuery{}, &NoQueryTermsError{QueryID: q.ID, Reason: err.Error()}
		}
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

// parseBoolean reads text with the syntax the API's boolean search takes: AND,
// OR and NOT in any case, parentheses, quoted phrases, and AND between
// adjacent operands. NOT binds tighter than AND, and AND tighter than OR. It
// returns the expression and the operands that are not negated.
func parseBoolean(text string) (docMatcher, []string, error) {
	tokenizer := token.NewBoolTokenizer()
	tokens := tokenizer.Tokenize(text)
	if err := tokenizer.Validate(tokens); err != nil {
		return nil, nil, fmt.Errorf("boolean text %q: %w", text, err)
	}
	p := &booleanParser{tokens: tokens}
	expr, err := p.or(false)
	if err == nil && p.peek().Type != token.EOF {
		err = fmt.Errorf("unexpected %q", p.peek().Value)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("boolean text %q: %w", text, err)
	}
	return expr, p.positives, nil
}

type booleanParser struct {
	tokens    []token.Token
	pos       int
	positives []string
}

func (p *booleanParser) peek() token.Token { return p.tokens[p.pos] }

func (p *booleanParser) next() token.Token {
	tok := p.tokens[p.pos]
	if tok.Type != token.EOF {
		p.pos++
	}
	return tok
}

func (p *booleanParser) or(negated bool) (docMatcher, error) {
	operands := anyOf{}
	for {
		operand, err := p.and(negated)
		if err != nil {
			return nil, err
		}
		operands = append(operands, operand)
		if p.peek().Type != token.OR {
			break
		}
		p.next()
	}
	if len(operands) == 1 {
		return operands[0], nil
	}
	return operands, nil
}

func (p *booleanParser) and(negated bool) (docMatcher, error) {
	operands := allOf{}
	for {
		operand, err := p.unary(negated)
		if err != nil {
			return nil, err
		}
		operands = append(operands, operand)
		switch p.peek().Type {
		case token.AND:
			p.next()
			continue
		case token.WORD, token.LPAREN, token.NOT:
			continue
		}
		break
	}
	if len(operands) == 1 {
		return operands[0], nil
	}
	return operands, nil
}

func (p *booleanParser) unary(negated bool) (docMatcher, error) {
	if p.peek().Type == token.NOT {
		p.next()
		operand, err := p.unary(!negated)
		if err != nil {
			return nil, err
		}
		return notMatcher{operand: operand}, nil
	}
	return p.primary(negated)
}

func (p *booleanParser) primary(negated bool) (docMatcher, error) {
	tok := p.next()
	switch tok.Type {
	case token.WORD:
		phrase := terms(tok.Value)
		if len(phrase) == 0 {
			return nil, fmt.Errorf("operand %q has no terms", tok.Value)
		}
		if !negated {
			p.positives = append(p.positives, tok.Value)
		}
		if len(phrase) == 1 {
			return termMatcher(phrase[0]), nil
		}
		return phraseMatcher(phrase), nil
	case token.LPAREN:
		expr, err := p.or(negated)
		if err != nil {
			return nil, err
		}
		if p.next().Type != token.RPAREN {
			return nil, fmt.Errorf("unclosed parenthesis")
		}
		return expr, nil
	case token.EOF:
		return nil, fmt.Errorf("ends where an operand belongs")
	default:
		return nil, fmt.Errorf("unexpected %q", tok.Value)
	}
}
