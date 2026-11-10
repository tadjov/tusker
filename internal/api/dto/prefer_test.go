package dto

import (
	"testing"

	"github.com/tadjov/tusker/internal/types/query"
)

func TestHandlingFromPrefer(t *testing.T) {
	tests := []struct {
		name   string
		header []string
		want   query.Handling
	}{
		{name: "no Prefer header", header: nil, want: query.HandlingStrict},
		{name: "lenient", header: []string{"handling=lenient"}, want: query.HandlingLenient},
		{name: "strict", header: []string{"handling=strict"}, want: query.HandlingStrict},
		{name: "case and spacing", header: []string{" Handling = LENIENT "}, want: query.HandlingLenient},
		{name: "quoted value", header: []string{`handling="lenient"`}, want: query.HandlingLenient},
		{name: "among other preferences", header: []string{"respond-async, handling=lenient; foo=bar, wait=10"}, want: query.HandlingLenient},
		{name: "in a second header line", header: []string{"respond-async", "handling=lenient"}, want: query.HandlingLenient},
		{name: "first occurrence wins", header: []string{"handling=strict, handling=lenient"}, want: query.HandlingStrict},
		{name: "an unknown value is ignored", header: []string{"handling=relaxed"}, want: query.HandlingStrict},
		{name: "other preferences only", header: []string{"return=minimal"}, want: query.HandlingStrict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HandlingFromPrefer(tt.header); got != tt.want {
				t.Errorf("HandlingFromPrefer(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}
