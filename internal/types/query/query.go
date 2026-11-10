package query

import (
	"fmt"

	"github.com/tadjov/tusker/internal/types/language"
	"github.com/tadjov/tusker/internal/types/operator"
)

// Kind QueryType represents the search paradigm to use
type Kind string

const (
	// StringType QueryTypeQueryString: Simple text-based search query (application-optimized)
	StringType Kind = "query_string"

	// MatchType: Single-field match query (Elasticsearch terminology)
	// ES: match query on single field
	// PG: tsvector search on single field
	MatchType Kind = "match"

	// MultiMatchType: Multi-field match query (Elasticsearch terminology)
	// ES: multi_match query with field boosting
	// PG: weighted tsvector search across multiple fields
	MultiMatchType Kind = "multi_match"

	// BooleanType: Structured queries with logical operators (AND, OR, NOT)
	BooleanType Kind = "boolean"

	// PhraseType: Phrase match query with optional slop
	// ES: match_phrase query with slop parameter
	// PG: phraseto_tsquery (slop=0) or to_tsquery with <N> operators (slop>0)
	PhraseType Kind = "phrase"

	// HybridType: lexical FTS fused with vector similarity via RRF.
	HybridType Kind = "hybrid"
)

// Base is the top-level query container
// Only one query field should be non-nil based on Kind
type Base struct {
	Kind        Kind        `json:"kind"`
	QueryString *String     `json:"query_string,omitempty"`
	Match       *Match      `json:"match,omitempty"`
	MultiMatch  *MultiMatch `json:"multi_match,omitempty"`
	Boolean     *Boolean    `json:"boolean,omitempty"`
	Phrase      *Phrase     `json:"phrase,omitempty"`
	Hybrid      *Hybrid     `json:"hybrid,omitempty"`
}

// String is a free-text query whose fields, weights and operator come from the
// SearchContract rather than from the caller.
type String struct {
	Query string `json:"query" validate:"required,min=1"`

	// Language overrides the contract's analysis language when set.
	Language language.Code `json:"language,omitempty"`
}

// Contract returns the default search contract with the query's overrides applied.
func (q *String) Contract() SearchContract {
	contract := DefaultSearchContract()
	if q.Language != "" {
		contract.Language = q.Language
	}
	return contract
}

func (q *String) Effective(engineDefault operator.Operator) Effective {
	return q.Contract().Effective(engineDefault)
}

// Boolean is a structured query using logical operators.
type Boolean struct {
	// Expression: Boolean query string with operators
	// Supported operators:
	//   - AND (&): All terms must be present
	//   - OR (|): At least one term must be present
	//   - NOT (!): Term must not be present
	//   - (): Grouping for precedence
	//
	// Examples:
	//   "climate AND change"
	//   "(renewable OR sustainable) AND energy"
	//   "Trump AND NOT biden"
	//   "(climate OR weather) AND (change OR warming)"
	Expression string        `json:"expression" validate:"required,min=1"`
	Language   language.Code `json:"language,omitempty"`
}

// GetLanguage returns the language with default fallback
func (q *Boolean) GetLanguage() language.Code {
	if q.Language == "" {
		return DefaultLanguage
	}
	return q.Language
}

// Effective reports the contract fields, which an expression matches, and the
// operator that joins adjacent words.
func (q *Boolean) Effective(engineDefault operator.Operator) Effective {
	contract := DefaultSearchContract()
	contract.Language = q.GetLanguage()
	return contract.Effective(engineDefault)
}

// Phrase is a phrase match query with optional slop.
// Searches for an exact phrase with optional positional flexibility.
//
// Elasticsearch: Translates to {"match_phrase": {"field": {"query": "text", "slop": N}}}
// PostgreSQL: Uses phraseto_tsquery (slop=0) or to_tsquery with <N> operators (slop>0)
//
// Example:
//
//	{"query": "climate change", "fields": ["title", "content"], "slop": 2}
type Phrase struct {
	// Query: The exact phrase to search for
	Query string `json:"query" validate:"required,min=1"`

	// Fields: Fields to search in (supports multiple like multi_match)
	Fields []Field `json:"fields" validate:"required,min=1"`

	// Slop: Maximum positions allowed between matching tokens
	// 0 = exact phrase, 1 = one word between, etc.
	// Maximum value: 3 (to limit PostgreSQL OR clause complexity)
	// Default: 0 (exact phrase)
	Slop int `json:"slop,omitempty" validate:"min=0,max=3"`

	// Language: Prompt analysis language
	Language language.Code `json:"language,omitempty"`
}

const MaxPhraseSlop = 3

type PhraseOption func(q *Phrase)

// NewPhrase rejects empty text, no fields, a field outside SearchableFields and
// a slop above MaxPhraseSlop.
func NewPhrase(query string, fields []Field, opts ...PhraseOption) (*Phrase, error) {
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("fields are required")
	}
	for _, field := range fields {
		if _, err := ParseField(string(field)); err != nil {
			return nil, err
		}
	}

	q := &Phrase{
		Query:    query,
		Fields:   fields,
		Language: DefaultLanguage,
		Slop:     0, // Default to exact phrase
	}

	for _, opt := range opts {
		opt(q)
	}

	// Validate slop range
	if q.Slop < 0 || q.Slop > MaxPhraseSlop {
		return nil, fmt.Errorf("slop must be between 0 and %d, got %d", MaxPhraseSlop, q.Slop)
	}

	return q, nil
}

// WithPhraseSlop sets the slop (positional flexibility) for Phrase query
func WithPhraseSlop(slop int) PhraseOption {
	return func(q *Phrase) {
		q.Slop = slop
	}
}

// WithPhraseLanguage sets the language for Phrase query
func WithPhraseLanguage(lang language.Code) PhraseOption {
	return func(q *Phrase) {
		q.Language = lang
	}
}

// GetLanguage returns the language with default fallback
func (q *Phrase) GetLanguage() language.Code {
	if q.Language == "" {
		return DefaultLanguage
	}
	return q.Language
}

func (q *Phrase) Params() Params {
	return Params{Fields: unboostedFields(q.Fields), Slop: q.GetSlop()}
}

// Apply narrows the query to params, a subset of its own Params.
func (q *Phrase) Apply(params Params) {
	q.Fields = fieldWeights(params.Fields).fields()
	q.Slop = params.Slop
}

func (q *Phrase) Effective() Effective {
	return Effective{Fields: unboostedFields(q.Fields).fields(), Language: q.GetLanguage()}
}

// GetSlop returns the slop value (default 0)
func (q *Phrase) GetSlop() int {
	if q.Slop < 0 {
		return 0
	}
	if q.Slop > MaxPhraseSlop {
		return MaxPhraseSlop
	}
	return q.Slop
}

var (
	// DefaultFields are the default fields to search when not specified
	DefaultFields = []string{"title", "description", "content"}

	// DefaultFieldWeights are the default field weights (equal weighting)
	DefaultFieldWeights = map[string]float64{
		"title":       1.0,
		"description": 1.0,
		"content":     1.0,
	}
)

type StringOption func(q *String)

func NewQueryString(query string, opts ...StringOption) *String {
	q := &String{Query: query}
	for _, opt := range opts {
		opt(q)
	}
	return q
}

func WithQueryStringLanguage(lang language.Code) StringOption {
	return func(q *String) {
		q.Language = lang
	}
}

// Match is a single-field match query.
// Performs analyzed full-text search on a single field with relevance scoring
// Elasticsearch: Translates to {"match": {"field": {"query": "text"}}}
// PostgreSQL: Uses weighted tsvector on single field
// Example:
//
//	{"field": "title", "query": "climate change", "operator": "and"}
type Match struct {
	// Query: The text to search for (analyzed and tokenized)
	Query string `json:"query" validate:"required,min=1"`

	// Field: The single field to search in
	Field Field `json:"field" validate:"required"`

	// Language: Prompt search language configuration
	Language language.Code `json:"language,omitempty"`

	// Operator combines the terms; nil leaves the engine's default.
	Operator *operator.Operator `json:"operator,omitempty"`

	// Fuzziness: Typo tolerance (general search concept)
	// Elasticsearch: Native support via fuzziness parameter
	// PostgreSQL: Not supported; see Capabilities
	Fuzziness Fuzziness `json:"fuzziness,omitempty"`
}

// GetLanguage returns the language with default fallback
func (q *Match) GetLanguage() language.Code {
	if q.Language == "" {
		return DefaultLanguage
	}
	return q.Language
}

func (q *Match) Params() Params {
	return Params{Fields: unboostedFields([]Field{q.Field}), Fuzziness: q.GetFuzziness()}
}

// Apply narrows the query to params, a subset of its own Params. Its one field
// always survives: a check that would drop it rejects the request instead.
func (q *Match) Apply(params Params) {
	q.Fuzziness = params.Fuzziness
}

func (q *Match) Effective(engineDefault operator.Operator) Effective {
	return Effective{
		Operator: operator.Resolve(q.Operator, engineDefault),
		Fields:   []Field{q.Field},
		Language: q.GetLanguage(),
	}
}

func (q *Match) GetFuzziness() Fuzziness {
	if q.Fuzziness == "" {
		return NoFuzziness
	}
	return q.Fuzziness
}

type MatchQueryOption func(q *Match)

// NewMatch rejects empty text and a field outside SearchableFields.
func NewMatch(field Field, query string, opts ...MatchQueryOption) (*Match, error) {
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if _, err := ParseField(string(field)); err != nil {
		return nil, err
	}
	q := &Match{
		Field:     field,
		Query:     query,
		Language:  DefaultLanguage,
		Fuzziness: NoFuzziness,
	}

	for _, opt := range opts {
		opt(q)
	}

	return q, nil
}

// WithMatchLanguage sets the language for Match query
func WithMatchLanguage(lang language.Code) MatchQueryOption {
	return func(q *Match) {
		q.Language = lang
	}
}

// WithMatchOperator sets the operator for Match query
func WithMatchOperator(op operator.Operator) MatchQueryOption {
	return func(q *Match) {
		q.Operator = &op
	}
}

// WithMatchFuzziness sets the fuzziness for Match query
func WithMatchFuzziness(fuzziness Fuzziness) MatchQueryOption {
	return func(q *Match) {
		q.Fuzziness = fuzziness
	}
}

type MultiMatchField struct {
	Name   Field
	Weight float64
}

func NewMultiMatchField(name Field) MultiMatchField {
	return MultiMatchField{
		Name:   name,
		Weight: 1.0,
	}
}

func NewMultiMatchBoostedField(name Field, weight float64) MultiMatchField {
	return MultiMatchField{
		Name:   name,
		Weight: weight,
	}
}

type MultiMatchStrategy string

const (
	// MultiMatchBestFields MultiMatchTypeBestFields: Finds documents that match any field, but uses the _best_ matching field to score each document.
	// For now, only this strategy is supported.
	MultiMatchBestFields MultiMatchStrategy = "best_fields"
)

// MultiMatch is a multi-field match query (Elasticsearch terminology).
// Performs analyzed full-text search across multiple fields with per-field boosting
//
// Elasticsearch: Translates to {"multi_match": {"query": "text", "fields": ["title^3", "content"]}}
// PostgreSQL: Uses weighted tsvector with custom field weights
//
// Example:
//
//	{"query": "climate change", "fields": ["title^3", "content"]}
type MultiMatch struct {
	// Query: The text to search for (analyzed and tokenized)
	Query string `json:"query" validate:"required,min=1"`

	Fields []MultiMatchField `json:"fields,omitempty"`

	// Language: Prompt search language configuration
	Language language.Code `json:"language,omitempty"`

	// Operator combines the terms; nil leaves the engine's default.
	Operator *operator.Operator `json:"operator,omitempty"`

	MatchStrategy MultiMatchStrategy `json:"match_strategy,omitempty"`
}
type MultiMatchQueryOption func(q *MultiMatch)

func NewMultiMatchQuery(query string, fields []string, opts ...MultiMatchQueryOption) (*MultiMatch, error) {
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("fields are required")
	}

	parsedFields, err := newMultiMatchFields(fields)
	if err != nil {
		return nil, err
	}

	q := &MultiMatch{
		Query:         query,
		Language:      DefaultLanguage,
		MatchStrategy: MultiMatchBestFields,
		Fields:        parsedFields,
	}

	for _, opt := range opts {
		opt(q)
	}

	return q, nil
}

func WithMultiMatchLanguage(lang language.Code) MultiMatchQueryOption {
	return func(q *MultiMatch) {
		q.Language = lang
	}
}

func WithMultiMatchOperator(op operator.Operator) MultiMatchQueryOption {
	return func(q *MultiMatch) {
		q.Operator = &op
	}
}

func newMultiMatchFields(specs []string) ([]MultiMatchField, error) {
	fields := make([]MultiMatchField, 0, len(specs))
	for _, spec := range specs {
		fieldBoost, err := ParseFieldBoost(spec)
		if err != nil {
			return nil, err
		}
		fields = append(fields, NewMultiMatchBoostedField(fieldBoost.Field, fieldBoost.Weight))
	}
	return fields, nil
}

func (q *MultiMatch) GetLanguage() language.Code {
	if q.Language == "" {
		return DefaultLanguage
	}
	return q.Language
}

func (q *MultiMatch) Params() Params {
	return Params{Fields: q.fieldWeights()}
}

// Apply narrows the query to params, a subset of its own Params.
func (q *MultiMatch) Apply(params Params) {
	fields := make([]MultiMatchField, 0, len(params.Fields))
	for _, f := range params.Fields {
		fields = append(fields, NewMultiMatchBoostedField(f.Field, f.Weight))
	}
	q.Fields = fields
}

func (q *MultiMatch) Effective(engineDefault operator.Operator) Effective {
	return Effective{
		Operator: operator.Resolve(q.Operator, engineDefault),
		Fields:   q.fieldWeights().fields(),
		Language: q.GetLanguage(),
	}
}

func (q *MultiMatch) fieldWeights() fieldWeights {
	weights := make(fieldWeights, 0, len(q.Fields))
	for _, f := range q.Fields {
		weights = append(weights, FieldWeight{Field: f.Name, Weight: f.Weight})
	}
	return weights
}

func (q *MultiMatch) GetFields() []MultiMatchField {
	return q.Fields
}

type Semantic struct {
	// Query: The text to semantically search for
	Query     string  `json:"query" validate:"required,min=1"`
	Threshold float64 `json:"threshold" validate:"omitempty,gte=0,lte=2"`
}

func NewSemantic(query string) *Semantic {
	return &Semantic{
		Query: query,
	}
}

// DefaultRRFConstant is the standard RRF constant (k=60 per Cormack et al.).
const DefaultRRFConstant = 60

// Hybrid fuses lexical FTS with vector similarity via RRF.
// score = 1/(k + lex_rank) + 1/(k + vec_rank)
type Hybrid struct {
	Query string `json:"query" validate:"required,min=1"`

	Language language.Code `json:"language,omitempty"`

	// K is the RRF constant; higher values flatten rank contribution differences.
	K int `json:"k,omitempty"`
}

type HybridOption func(q *Hybrid)

func NewHybrid(query string, opts ...HybridOption) *Hybrid {
	q := &Hybrid{
		Query:    query,
		Language: DefaultLanguage,
		K:        DefaultRRFConstant,
	}

	for _, opt := range opts {
		opt(q)
	}

	return q
}

func WithHybridLanguage(lang language.Code) HybridOption {
	return func(q *Hybrid) {
		q.Language = lang
	}
}

func WithHybridK(k int) HybridOption {
	return func(q *Hybrid) {
		q.K = k
	}
}

func (q *Hybrid) GetLanguage() language.Code {
	if q.Language == "" {
		return DefaultLanguage
	}
	return q.Language
}

// Effective leaves Fields empty: each engine's lexical leg picks its own.
func (q *Hybrid) Effective(engineDefault operator.Operator) Effective {
	return Effective{Operator: engineDefault, Language: q.GetLanguage()}
}

func (q *Hybrid) GetK() int {
	if q.K <= 0 {
		return DefaultRRFConstant
	}
	return q.K
}
