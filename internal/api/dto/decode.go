package dto

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"sort"

	"github.com/tadjov/tusker/internal/apperr"
)

// DecodeSearchRequest reads a structured search request. A key no request type
// defines is rejected, so a stated parameter is never silently dropped.
func DecodeSearchRequest(body io.Reader) (*SearchRequest, error) {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req SearchRequest
	if err := dec.Decode(&req); err != nil {
		var validationErr *apperr.ValidationError
		if errors.As(err, &validationErr) {
			return nil, err
		}
		return nil, apperr.NewValidationWrap("invalid request body: "+err.Error(), err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, apperr.NewValidationWrap("invalid request body: unexpected data after the JSON object", err)
	}
	return &req, nil
}

var searchBoxParams = []string{"q", "query", "size", "cursor", "lang"}

// CheckSearchBoxParams rejects a query parameter the search box does not take,
// so a stated one is never silently dropped.
func CheckSearchBoxParams(params url.Values) error {
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !slices.Contains(searchBoxParams, name) {
			return apperr.NewValidation(fmt.Sprintf("unsupported query parameter %q (the search box takes q, size, cursor and lang)", name))
		}
	}
	return nil
}
