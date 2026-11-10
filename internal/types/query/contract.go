package query

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/tadjov/tusker/internal/types/language"
	"github.com/tadjov/tusker/internal/types/operator"
)

// Field is an article field a query may search.
type Field string

const (
	FieldTitle       Field = "title"
	FieldDescription Field = "description"
	FieldContent     Field = "content"
)

// SearchableFields is the allow-list of fields every engine indexes for full-text search.
func SearchableFields() []Field {
	return []Field{FieldTitle, FieldDescription, FieldContent}
}

func ParseField(s string) (Field, error) {
	searchable := SearchableFields()
	if !slices.Contains(searchable, Field(s)) {
		return "", fmt.Errorf("unsupported field: %q (must be one of %v)", s, searchable)
	}
	return Field(s), nil
}

type FieldWeight struct {
	Field  Field
	Weight float64
}

// ParseFieldBoost parses "field" or "field^boost". An unboosted field weighs 1;
// a boost must be a positive number both engines can hold as a float32.
func ParseFieldBoost(spec string) (FieldWeight, error) {
	name, rawBoost, boosted := strings.Cut(strings.TrimSpace(spec), "^")
	field, err := ParseField(name)
	if err != nil {
		return FieldWeight{}, err
	}
	if !boosted {
		return FieldWeight{Field: field, Weight: 1}, nil
	}
	boost, err := strconv.ParseFloat(rawBoost, 64)
	if err != nil || math.IsNaN(boost) || boost < math.SmallestNonzeroFloat32 || boost > math.MaxFloat32 {
		return FieldWeight{}, fmt.Errorf("invalid boost in %q: must be between %g and %g", spec, math.SmallestNonzeroFloat32, math.MaxFloat32)
	}
	return FieldWeight{Field: field, Weight: boost}, nil
}

// SearchContract is the question a query string asks every engine: which fields
// to match, how to combine terms and how to analyse them. Engines build their
// query from it. A nil Operator leaves each engine's default in charge, so the
// recall differs by engine exactly as their defaults do. Field weights are
// ranking boosts, and an engine may rank with its own weights instead.
type SearchContract struct {
	Fields   []FieldWeight
	Operator *operator.Operator
	Language language.Code
}

// Effective is the contract a search ran under once the engine filled in what
// the request left out. No Operator means the query type has none; no Fields
// means the engine chose them.
type Effective struct {
	Operator operator.Operator `json:"operator,omitempty"`
	Fields   []Field           `json:"fields,omitempty"`
	Language language.Code     `json:"language"`
}

// Effective returns the contract with a nil Operator resolved to engineDefault.
func (c SearchContract) Effective(engineDefault operator.Operator) Effective {
	return Effective{
		Operator: operator.Resolve(c.Operator, engineDefault),
		Fields:   fieldWeights(c.Fields).fields(),
		Language: c.Language,
	}
}

// DefaultSearchContract is the query-string search of the API: the terms match
// the title, description or content under the engine's default operator,
// analysed in English, with title boosted 3 and description 2.
func DefaultSearchContract() SearchContract {
	return SearchContract{
		Fields: []FieldWeight{
			{Field: FieldTitle, Weight: 3.0},
			{Field: FieldDescription, Weight: 2.0},
			{Field: FieldContent, Weight: 1.0},
		},
		Language: DefaultLanguage,
	}
}

type fieldWeights []FieldWeight

func unboostedFields(fields []Field) fieldWeights {
	weights := make(fieldWeights, 0, len(fields))
	for _, field := range fields {
		weights = append(weights, FieldWeight{Field: field, Weight: 1})
	}
	return weights
}

func (w fieldWeights) fields() []Field {
	fields := make([]Field, 0, len(w))
	for _, fw := range w {
		fields = append(fields, fw.Field)
	}
	return fields
}
