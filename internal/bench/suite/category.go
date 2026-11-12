package suite

import (
	"fmt"
	"slices"
)

// Category is a query's kind. It drives per-category aggregation, and the
// judges grade phrase and boolean queries by their constraint.
type Category string

const (
	CategoryKeyword     Category = "keyword"
	CategoryPhrase      Category = "phrase"
	CategoryBoolean     Category = "boolean"
	CategoryMultiField  Category = "multi-field"
	CategorySingleField Category = "single-field"
)

var categories = []Category{CategoryKeyword, CategoryPhrase, CategoryBoolean, CategoryMultiField, CategorySingleField}

func ParseCategory(s string) (Category, error) {
	if c := Category(s); slices.Contains(categories, c) {
		return c, nil
	}
	return "", fmt.Errorf("unknown category %q (known: %v)", s, categories)
}
