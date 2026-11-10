package dto

import (
	"errors"
	"strings"
	"testing"

	"github.com/tadjov/tusker/internal/apperr"
	"github.com/tadjov/tusker/pkg/pagination"
)

func isValidationError(err error) bool {
	var validationErr *apperr.ValidationError
	return errors.As(err, &validationErr)
}

func TestToDomain_BoundsQueryLength(t *testing.T) {
	atLimit := strings.Repeat("a", MaxQueryLength)
	overLimit := strings.Repeat("a", MaxQueryLength+1)
	// Cyrillic letters are two bytes each, so this is over the limit in bytes only.
	cyrillicAtLimit := strings.Repeat("ж", MaxQueryLength)

	conversions := map[string]func(text string) error{
		"match": func(text string) error {
			_, err := (&MatchParams{Query: text, Field: "title"}).ToDomain()
			return err
		},
		"multi_match": func(text string) error {
			_, err := (&MultiMatchParams{Query: text, Fields: []string{"title"}}).ToDomain()
			return err
		},
		"phrase": func(text string) error {
			_, err := (&PhraseParams{Query: text, Fields: []string{"title"}}).ToDomain()
			return err
		},
		"boolean": func(text string) error {
			_, err := (&BooleanParams{Expression: text}).ToDomain()
			return err
		},
		"hybrid": func(text string) error {
			_, err := (&HybridParams{Query: text}).ToDomain()
			return err
		},
		"semantic": func(text string) error {
			_, err := (&SemanticSearchRequest{Query: text}).ToDomain()
			return err
		},
		"query_string": func(text string) error {
			_, err := (&StringQueryParams{Query: text}).ToDomain()
			return err
		},
	}

	for name, toDomain := range conversions {
		t.Run(name, func(t *testing.T) {
			if err := toDomain(atLimit); err != nil {
				t.Errorf("query of %d characters: %v, want accepted", MaxQueryLength, err)
			}
			if err := toDomain(cyrillicAtLimit); err != nil {
				t.Errorf("Cyrillic query of %d characters: %v, want accepted", MaxQueryLength, err)
			}
			if err := toDomain(overLimit); !isValidationError(err) {
				t.Errorf("query of %d characters: %v, want a validation error", MaxQueryLength+1, err)
			}
		})
	}
}

func TestPageSize(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		want      int
		wantErr   bool
	}{
		{name: "unset takes the default", requested: 0, want: pagination.PageDefaultSize},
		{name: "within bounds", requested: 25, want: 25},
		{name: "at the maximum", requested: pagination.PageMaxSize, want: pagination.PageMaxSize},
		{name: "over the maximum", requested: pagination.PageMaxSize + 1, wantErr: true},
		{name: "negative", requested: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PageSize(tt.requested)
			if tt.wantErr {
				if !isValidationError(err) {
					t.Errorf("PageSize(%d) error = %v, want a validation error", tt.requested, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("PageSize(%d) = %d, %v; want %d", tt.requested, got, err, tt.want)
			}
		})
	}
}

func TestParsePageSize(t *testing.T) {
	tests := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{raw: "", want: pagination.PageDefaultSize},
		{raw: "25", want: 25},
		{raw: "0", wantErr: true},
		{raw: "ten", wantErr: true},
		{raw: "10001", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := ParsePageSize(tt.raw)
			if tt.wantErr {
				if !isValidationError(err) {
					t.Errorf("ParsePageSize(%q) error = %v, want a validation error", tt.raw, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("ParsePageSize(%q) = %d, %v; want %d", tt.raw, got, err, tt.want)
			}
		})
	}
}
