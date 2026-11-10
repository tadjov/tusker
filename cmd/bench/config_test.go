package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/runner"
	"github.com/tadjov/tusker/internal/bench/spec"
	"github.com/tadjov/tusker/internal/bench/trackctx"
	"github.com/tadjov/tusker/internal/storage"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

type stubVectorStore struct{ model string }

func (stubVectorStore) QueryVector(context.Context, string) ([]float32, error) { return nil, nil }
func (stubVectorStore) DocVectors(context.Context, []uuid.UUID) (map[uuid.UUID][]float32, error) {
	return nil, nil
}
func (s stubVectorStore) Model() string { return s.model }

func TestRequireEmbedder(t *testing.T) {
	t.Run("semantic without store fails", func(t *testing.T) {
		err := requireEmbedder(&spec.BenchSpec{Kind: spec.KindSemantic}, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "EMBEDDING_BASE_URL")
	})

	t.Run("semantic with store ok", func(t *testing.T) {
		var store storage.VectorStore = stubVectorStore{}
		require.NoError(t, requireEmbedder(&spec.BenchSpec{Kind: spec.KindSemantic}, store))
	})

	t.Run("non-vector kind without store ok", func(t *testing.T) {
		require.NoError(t, requireEmbedder(&spec.BenchSpec{Kind: spec.KindFTS}, nil))
	})

	t.Run("empty kind without store ok", func(t *testing.T) {
		require.NoError(t, requireEmbedder(&spec.BenchSpec{}, nil))
	})
}

// An interrupt ends a glob run at the track it cut short; the remaining tracks
// would otherwise run against a cancelled context and all report failure.
func TestForEachTrack_InterruptStopsGlob(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		dir := filepath.Join(root, "g", name)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "trec"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "spec.yaml"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "suite.yaml"), nil, 0o644))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ran []string
	err := forEachTrack(ctx, io.Discard, trackctx.Inputs{TrackArg: "g/*", TrackRoot: root}, func(tr *trackctx.Track) error {
		ran = append(ran, tr.Name())
		cancel()
		return errors.New("30 query/engine pair(s) failed validation")
	})

	require.Error(t, err)
	assert.Len(t, ran, 1)
}

const queryVectorsFixtureTag = "qwen3-embedding:0.6b@1024"

// queryVectorsTrack copies the query-vectors fixture track and gives it a spec
// whose postgres engine declares the package's model.
func queryVectorsTrack(t *testing.T, pgConn string) string {
	t.Helper()
	track := copyQueryVectorsTrack(t)
	require.NoError(t, os.WriteFile(filepath.Join(track, "spec.yaml"), fmt.Appendf(nil, `schema_version: 1
id: fixture-semantic
kind: semantic
engines:
  pg:
    type: postgres
    connection: %q
    params:
      embedding_model: %q
jobs:
  - name: knn
    suite: suite.yaml
    engines: [pg]
`, pgConn, queryVectorsFixtureTag), 0o644))
	return track
}

func copyQueryVectorsTrack(t *testing.T) string {
	t.Helper()
	track := t.TempDir()
	src := filepath.Join("..", "..", "internal", "storage", "queryvectors", "fixtures", "track")
	require.NoError(t, os.CopyFS(track, os.DirFS(src)))
	return track
}

// Following VerifyEmbeddingModel's remedy for a spec declaring another width
// must give a store that passes it.
func TestBuildQueryVectorStore_OllamaStoreTagsTheConfiguredWidth(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewPGContainerWithCleanup(ctx, t)
	t.Setenv("EMBEDDING_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("EMBEDDING_MODEL", "qwen3-embedding:0.6b")
	t.Setenv("EMBEDDING_DIM", "768")
	bs := &spec.BenchSpec{Engines: map[string]spec.Engine{"pg": {
		Type:       spec.EnginePostgres,
		Connection: container.ConnString,
		Params:     map[string]any{"embedding_model": "qwen3-embedding:0.6b@768"},
	}}}

	store, closeStore, err := buildQueryVectorStore(ctx, bs, &trackctx.Track{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(closeStore)

	require.NoError(t, runner.VerifyEmbeddingModel(bs.Engines, store))
}

func TestBuildQueryVectorStore_RefusesAPackageItCannotCheck(t *testing.T) {
	t.Setenv("EMBEDDING_BASE_URL", "")
	declaring := func(model string) spec.Engine {
		return spec.Engine{Type: spec.EngineElasticsearch, Params: map[string]any{"embedding_model": model}}
	}
	tests := []struct {
		name    string
		spec    func(track string) *spec.BenchSpec
		wantErr string
	}{
		{
			name: "jobs run two suites",
			spec: func(track string) *spec.BenchSpec {
				return &spec.BenchSpec{
					Engines: map[string]spec.Engine{"es": declaring(queryVectorsFixtureTag)},
					Jobs:    []spec.Job{{Suite: filepath.Join(track, "suite.yaml")}, {Suite: filepath.Join(track, "other.yaml")}},
				}
			},
			wantErr: "other.yaml",
		},
		{
			name: "engines declare different models",
			spec: func(track string) *spec.BenchSpec {
				return &spec.BenchSpec{
					Engines: map[string]spec.Engine{"a": declaring(queryVectorsFixtureTag), "b": declaring("qwen3-embedding:0.6b@768")},
					Jobs:    []spec.Job{{Suite: filepath.Join(track, "suite.yaml")}},
				}
			},
			wantErr: "@768",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			track := copyQueryVectorsTrack(t)

			store, closeStore, err := buildQueryVectorStore(context.Background(), tt.spec(track), &trackctx.Track{Root: track})
			t.Cleanup(closeStore)

			require.ErrorContains(t, err, tt.wantErr)
			assert.Nil(t, store)
		})
	}
}
