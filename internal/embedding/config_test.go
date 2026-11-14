package embedding

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigFromEnv_Dim(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{name: "unset is the native 1024", raw: "", want: 1024},
		{name: "positive width", raw: "512", want: 512},
		{name: "zero would store empty vectors", raw: "0", wantErr: true},
		{name: "negative would panic mid-load", raw: "-1", wantErr: true},
		{name: "not a number", raw: "1k", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("EMBEDDING_ENABLED", "")
			t.Setenv("EMBEDDING_DIM", tt.raw)

			cfg, err := LoadConfigFromEnv()

			if tt.wantErr {
				require.ErrorContains(t, err, "EMBEDDING_DIM")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.Dim)
		})
	}
}

func TestLoadConfigFromEnv_RefusesTheRenamedMaxLength(t *testing.T) {
	t.Setenv("EMBEDDING_ENABLED", "")
	t.Setenv("EMBEDDING_MAX_LENGTH", "512")

	_, err := LoadConfigFromEnv()

	require.ErrorContains(t, err, "EMBEDDING_DIM")
}
