package query

import (
	"errors"
	"slices"
	"testing"

	"github.com/tadjov/tusker/internal/types/operator"
)

var (
	partialFields = Capabilities{
		Engine:          "partial",
		DefaultOperator: operator.And,
		PhraseSlop:      true,
		Boosts:          true,
		Fields:          []Field{FieldTitle, FieldDescription},
	}
	elasticsearchLike = Capabilities{
		Engine:          "elasticsearch",
		DefaultOperator: operator.Or,
		Fuzziness:       true,
		PhraseSlop:      true,
		Boosts:          true,
		Fields:          SearchableFields(),
	}
)

func TestCheckCapabilities(t *testing.T) {
	title := FieldWeight{Field: FieldTitle, Weight: 1}
	tests := []struct {
		name         string
		caps         Capabilities
		params       Params
		handling     Handling
		wantErrFor   string
		wantIgnored  []string
		wantHonoured Params
	}{
		{
			name:         "every parameter honoured",
			caps:         elasticsearchLike,
			params:       Params{Fields: []FieldWeight{{Field: FieldContent, Weight: 2}}, Fuzziness: FuzzinessAuto, Slop: 2},
			handling:     HandlingStrict,
			wantHonoured: Params{Fields: []FieldWeight{{Field: FieldContent, Weight: 2}}, Fuzziness: FuzzinessAuto, Slop: 2},
		},
		{
			name:       "strict rejects fuzziness the engine lacks",
			caps:       partialFields,
			params:     Params{Fields: []FieldWeight{title}, Fuzziness: FuzzinessAuto},
			handling:   HandlingStrict,
			wantErrFor: "fuzziness",
		},
		{
			name:         "lenient drops fuzziness the engine lacks",
			caps:         partialFields,
			params:       Params{Fields: []FieldWeight{title}, Fuzziness: Fuzziness1},
			handling:     HandlingLenient,
			wantIgnored:  []string{"fuzziness"},
			wantHonoured: Params{Fields: []FieldWeight{title}, Fuzziness: NoFuzziness},
		},
		{
			name:         "no fuzziness is not a fuzziness request",
			caps:         partialFields,
			params:       Params{Fields: []FieldWeight{title}, Fuzziness: NoFuzziness},
			handling:     HandlingStrict,
			wantHonoured: Params{Fields: []FieldWeight{title}, Fuzziness: NoFuzziness},
		},
		{
			name:         "an edit distance of zero is exact matching",
			caps:         partialFields,
			params:       Params{Fields: []FieldWeight{title}, Fuzziness: Fuzziness0},
			handling:     HandlingStrict,
			wantHonoured: Params{Fields: []FieldWeight{title}, Fuzziness: Fuzziness0},
		},
		{
			name:       "strict rejects a field the engine cannot isolate",
			caps:       partialFields,
			params:     Params{Fields: []FieldWeight{title, {Field: FieldContent, Weight: 1}}},
			handling:   HandlingStrict,
			wantErrFor: "fields",
		},
		{
			name:         "lenient drops a field the engine cannot isolate, boost and all",
			caps:         partialFields,
			params:       Params{Fields: []FieldWeight{title, {Field: FieldContent, Weight: 2}}},
			handling:     HandlingLenient,
			wantIgnored:  []string{"fields"},
			wantHonoured: Params{Fields: []FieldWeight{title}},
		},
		{
			name:       "lenient rejects dropping every field",
			caps:       partialFields,
			params:     Params{Fields: []FieldWeight{{Field: FieldContent, Weight: 1}}},
			handling:   HandlingLenient,
			wantErrFor: "fields",
		},
		{
			name:         "a boost on a field the engine isolates is honoured",
			caps:         partialFields,
			params:       Params{Fields: []FieldWeight{{Field: FieldTitle, Weight: 3}, {Field: FieldDescription, Weight: 1}}},
			handling:     HandlingStrict,
			wantHonoured: Params{Fields: []FieldWeight{{Field: FieldTitle, Weight: 3}, {Field: FieldDescription, Weight: 1}}},
		},
		{
			name:         "a boost is ranking only, so strict drops it rather than reject",
			caps:         Capabilities{Engine: "plain", Fields: SearchableFields()},
			params:       Params{Fields: []FieldWeight{{Field: FieldTitle, Weight: 2}}},
			handling:     HandlingStrict,
			wantIgnored:  []string{"boost"},
			wantHonoured: Params{Fields: []FieldWeight{title}},
		},
		{
			name:       "strict rejects slop the engine lacks",
			caps:       Capabilities{Engine: "plain", Fields: SearchableFields()},
			params:     Params{Fields: []FieldWeight{title}, Slop: 1},
			handling:   HandlingStrict,
			wantErrFor: "slop",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			honoured, ignored, err := CheckCapabilities(tt.caps, tt.params, tt.handling)
			if tt.wantErrFor != "" {
				var unsupported *UnsupportedParamError
				if !errors.As(err, &unsupported) {
					t.Fatalf("err = %v, want an UnsupportedParamError", err)
				}
				if unsupported.Param != tt.wantErrFor || unsupported.Engine != tt.caps.Engine {
					t.Errorf("unsupported %q on %q, want %q on %q", unsupported.Param, unsupported.Engine, tt.wantErrFor, tt.caps.Engine)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckCapabilities: %v", err)
			}
			var got []string
			for _, ig := range ignored {
				if ig.Engine != tt.caps.Engine {
					t.Errorf("ignored %q names engine %q, want %q", ig.Param, ig.Engine, tt.caps.Engine)
				}
				got = append(got, ig.Param)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.wantIgnored) {
				t.Errorf("ignored = %v, want %v", got, tt.wantIgnored)
			}
			if !slices.Equal(honoured.Fields, tt.wantHonoured.Fields) || honoured.Fuzziness != tt.wantHonoured.Fuzziness ||
				honoured.Slop != tt.wantHonoured.Slop {
				t.Errorf("honoured = %+v, want %+v", honoured, tt.wantHonoured)
			}
		})
	}
}

func TestParseHandling(t *testing.T) {
	tests := []struct {
		raw     string
		want    Handling
		wantErr bool
	}{
		{raw: "", want: HandlingStrict},
		{raw: "strict", want: HandlingStrict},
		{raw: "Lenient", want: HandlingLenient},
		{raw: "relaxed", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := ParseHandling(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseHandling(%q) = %q, want an error", tt.raw, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("ParseHandling(%q) = %q, %v; want %q", tt.raw, got, err, tt.want)
			}
		})
	}
}
