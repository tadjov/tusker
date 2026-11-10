package spec

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func specWithKValues(kValues string) []byte {
	return fmt.Appendf(nil, validSpecHeader+`defaults:
  pool_depth: 50
engines:
  pg:
    type: postgres
    connection: "postgresql://localhost/test"
metrics:
  k_values: %s
jobs:
  - name: pg
    suite: suite.yaml
    engines: [pg]
`, kValues)
}

func TestParse_KValuesMustBeScorableAtThePoolDepth(t *testing.T) {
	tests := []struct {
		name    string
		kValues string
		wantErr string
	}{
		{name: "repeated", kValues: "[10, 10]", wantErr: "cut-off 10"},
		{name: "zero", kValues: "[0]", wantErr: "cut-off 0"},
		{name: "negative", kValues: "[5, -1]", wantErr: "cut-off -1"},
		{name: "deeper than the pool", kValues: "[10, 100]", wantErr: "cut-off 100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(specWithKValues(tt.kValues))

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestParse_KValuesUpToThePoolDepthLoad(t *testing.T) {
	s, err := Parse(specWithKValues("[3, 10, 50]"))

	require.NoError(t, err)
	assert.Equal(t, []int{3, 10, 50}, s.Metrics.KValues)
}
