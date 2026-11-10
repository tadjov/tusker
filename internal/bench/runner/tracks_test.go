package runner

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/spec"
)

// Every committed track must load with each engine given the args its queries
// take; a broken suite would otherwise surface only when someone runs it.
func TestLoadSuites_CommittedTracksLoad(t *testing.T) {
	specs, err := filepath.Glob(filepath.Join("..", "..", "..", "tracks", "*", "*", "spec.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, specs)

	for _, path := range specs {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			bs, err := spec.LoadFromFile(path)
			require.NoError(t, err)

			_, err = LoadSuites(bs)

			require.NoError(t, err)
		})
	}
}
