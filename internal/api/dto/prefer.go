package dto

import (
	"strings"

	"github.com/tadjov/tusker/internal/types/query"
)

// HandlingFromPrefer reads the handling preference of RFC 7240 from the Prefer
// header values. Per the RFC an unrecognised value is ignored, which leaves
// the default, strict.
func HandlingFromPrefer(header []string) query.Handling {
	for _, line := range header {
		for _, preference := range strings.Split(line, ",") {
			token, _, _ := strings.Cut(preference, ";")
			name, value, _ := strings.Cut(token, "=")
			if !strings.EqualFold(strings.TrimSpace(name), "handling") {
				continue
			}
			handling, err := query.ParseHandling(strings.Trim(strings.TrimSpace(value), `"`))
			if err != nil {
				return query.HandlingStrict
			}
			return handling
		}
	}
	return query.HandlingStrict
}
