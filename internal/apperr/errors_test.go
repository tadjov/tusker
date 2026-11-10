package apperr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/tadjov/tusker/internal/apperr"
)

func TestNewValidation(t *testing.T) {
	err := apperr.NewValidation("field is required")

	if err.Error() != "field is required" {
		t.Errorf("expected 'field is required', got %q", err.Error())
	}
	if err.Unwrap() != nil {
		t.Errorf("expected nil unwrap, got %v", err.Unwrap())
	}
}

func TestNewValidationWrap(t *testing.T) {
	inner := fmt.Errorf("parse failed")
	err := apperr.NewValidationWrap("invalid expression", inner)

	if err.Error() != "invalid expression: parse failed" {
		t.Errorf("expected 'invalid expression: parse failed', got %q", err.Error())
	}
	if !errors.Is(err, inner) {
		t.Error("expected Unwrap to return inner error")
	}
}

func TestValidationError_SurvivesFmtWrapping(t *testing.T) {
	original := apperr.NewValidation("empty parentheses")

	wrapped := fmt.Errorf("failed to parse: %w", original)
	doubleWrapped := fmt.Errorf("storage error: %w", wrapped)

	var ve *apperr.ValidationError
	if !errors.As(doubleWrapped, &ve) {
		t.Fatal("errors.As should find ValidationError through double wrapping")
	}
	if ve.Message != "empty parentheses" {
		t.Errorf("expected 'empty parentheses', got %q", ve.Message)
	}
}

func TestValidationError_NotFoundForPlainErrors(t *testing.T) {
	plain := fmt.Errorf("database connection failed")
	wrapped := fmt.Errorf("storage error: %w", plain)

	var ve *apperr.ValidationError
	if errors.As(wrapped, &ve) {
		t.Fatal("errors.As should NOT find ValidationError in plain error chain")
	}
}
