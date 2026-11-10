package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DjordjeVuckovic/tusker/internal/bench/engine"
	"github.com/DjordjeVuckovic/tusker/internal/bench/spec"
	"github.com/DjordjeVuckovic/tusker/internal/bench/suite"
	"github.com/DjordjeVuckovic/tusker/internal/bench/trackctx"
	"github.com/DjordjeVuckovic/tusker/internal/datapackage"
	"github.com/DjordjeVuckovic/tusker/internal/embedding"
	"github.com/DjordjeVuckovic/tusker/internal/storage"
	"github.com/DjordjeVuckovic/tusker/internal/storage/factory"
	"github.com/DjordjeVuckovic/tusker/internal/storage/pg"
	"github.com/DjordjeVuckovic/tusker/internal/storage/queryvectors"
)

// trackRootEnv names the environment variable behind the --track-root flag.
const trackRootEnv = "BENCH_TRACK_ROOT"

// trackRoot is where relative track paths start (--track-root, defaulting to
// $BENCH_TRACK_ROOT). Empty means the current directory, so a track arg behaves
// like any other path; pointing it at a folder of tracks is pure convenience.
var trackRoot string

func createExecutors(ctx context.Context, bs *spec.BenchSpec) (map[string]engine.Executor, func(), error) {
	return engine.CreateFromSpec(ctx, bs.Engines)
}

// buildQueryVectorStore prefers the track's query-vectors package over live
// Ollama embedding; the store is nil when neither is available.
func buildQueryVectorStore(ctx context.Context, bs *spec.BenchSpec, tr *trackctx.Track) (storage.VectorStore, func(), error) {
	noop := func() {}
	packageDir, found, err := queryVectorsPackage(tr)
	if err != nil {
		return nil, noop, err
	}
	if found {
		return openQueryVectors(ctx, queryVectorsSource{spec: bs, packageDir: packageDir, postgres: postgresConnection(bs)})
	}

	baseURL := os.Getenv("EMBEDDING_BASE_URL")
	pgConn := postgresConnection(bs)
	if baseURL == "" || pgConn == "" {
		return nil, noop, nil
	}
	client, err := embedding.NewOllamaClient(baseURL)
	if err != nil {
		return nil, noop, fmt.Errorf("embedding client: %w", err)
	}
	dim, err := embedding.DimFromEnv()
	if err != nil {
		return nil, noop, err
	}
	store, err := factory.NewVectorStore(ctx, factory.VectorStoreConfig{
		PgConnStr:       pgConn,
		EmbeddingClient: client,
		Model:           os.Getenv("EMBEDDING_MODEL"),
		Dim:             dim,
	})
	return store, noop, err
}

func queryVectorsPackage(tr *trackctx.Track) (string, bool, error) {
	packageDir := filepath.Join(tr.Root, "trec", "query-vectors")
	_, err := os.Stat(filepath.Join(packageDir, datapackage.DescriptorName))
	switch {
	case err == nil:
		return packageDir, true, nil
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("look for query vectors in %s: %w", packageDir, err)
	}
}

type queryVectorsSource struct {
	spec       *spec.BenchSpec
	packageDir string
	// postgres serves document vectors; empty leaves them unreadable.
	postgres string
}

func openQueryVectors(ctx context.Context, src queryVectorsSource) (storage.VectorStore, func(), error) {
	noop := func() {}
	bs := src.spec
	suitePath, err := jobSuite(bs)
	if err != nil {
		return nil, noop, err
	}
	model, err := declaredEmbeddingModel(bs)
	if err != nil {
		return nil, noop, err
	}
	opts := queryvectors.Options{Package: src.packageDir, Suite: suitePath, Model: model}
	closePool := noop
	if conn := src.postgres; conn != "" {
		pool, err := pg.NewConnectionPool(ctx, pg.PoolConfig{ConnStr: conn, RegisterVecTypes: true})
		if err != nil {
			return nil, noop, fmt.Errorf("vector store: create PostgreSQL pool: %w", err)
		}
		opts.Postgres = pool
		closePool = pool.Close
	}
	store, err := queryvectors.Open(ctx, opts)
	if err != nil {
		closePool()
		return nil, noop, err
	}
	return store, closePool, nil
}

func jobSuite(bs *spec.BenchSpec) (string, error) {
	var suitePath string
	for _, job := range bs.Jobs {
		switch {
		case suitePath == "":
			suitePath = job.Suite
		case job.Suite != suitePath:
			return "", fmt.Errorf("jobs run suites %s and %s, but a query-vectors package covers one", suitePath, job.Suite)
		}
	}
	if suitePath == "" {
		return "", errors.New("spec has no job to take the query-vectors suite from")
	}
	return suitePath, nil
}

func declaredEmbeddingModel(bs *spec.BenchSpec) (string, error) {
	var model, declaredBy string
	for _, name := range slices.Sorted(maps.Keys(bs.Engines)) {
		declared, _ := bs.Engines[name].Params[suite.EmbeddingModelParam].(string)
		switch {
		case declared == "" || declared == model:
		case model == "":
			model, declaredBy = declared, name
		default:
			return "", fmt.Errorf("engines %q and %q declare different %s: %q and %q",
				declaredBy, name, suite.EmbeddingModelParam, model, declared)
		}
	}
	return model, nil
}

func postgresConnection(bs *spec.BenchSpec) string {
	for _, name := range slices.Sorted(maps.Keys(bs.Engines)) {
		if eng := bs.Engines[name]; eng.Type == spec.EnginePostgres {
			return eng.Connection
		}
	}
	return ""
}

func requireEmbedder(bs *spec.BenchSpec, store storage.VectorStore) error {
	if !bs.Kind.RequiresEmbedder() || store != nil {
		return nil
	}
	return fmt.Errorf("kind %q requires query vectors: add trec/query-vectors, or set EMBEDDING_BASE_URL and configure a postgres engine", bs.Kind)
}

// printSpecWarnings emits load-time advisories (e.g. kind omitted) once.
func printSpecWarnings(w io.Writer, bs *spec.BenchSpec) {
	for _, msg := range bs.Warnings {
		printWarn(w, msg)
	}
}

func parseKList(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		var v int
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%d", &v); err != nil {
			return nil, fmt.Errorf("invalid k value %q: %w", p, err)
		}
		if v <= 0 {
			return nil, fmt.Errorf("k value must be positive, got %d", v)
		}
		out = append(out, v)
	}
	return out, nil
}

func envOrFlag(envKey, flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envKey)
}

// trackInputs builds the resolution inputs shared by every subcommand: the
// track from a flag or positional arg, resolved against the active track root.
// Callers layer their own --spec/--pool/--output overrides on top.
func trackInputs(flag string, args []string) trackctx.Inputs {
	return trackctx.Inputs{TrackArg: trackArg(flag, args), TrackRoot: trackRoot}
}

// trackRootOrCwd is the track root with its default applied, for commands that
// build paths themselves instead of going through trackctx.
func trackRootOrCwd() string {
	if trackRoot == "" {
		return "."
	}
	return trackRoot
}

// trackArg picks up the track from a flag or first positional arg. The CLI
// allows either form: `bench pool tracks/news/fts` or `bench pool --track tracks/news/fts`.
func trackArg(flag string, args []string) string {
	if flag != "" {
		return flag
	}
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// forEachTrack runs fn once per track. A glob arg (news/*) fans out across every
// matching track; anything else is a single track. Grouping is explicit — only a
// glob expands. Path overrides (--spec/--pool/--output) target one track,
// so combining them with a glob is rejected. In group mode a per-track failure is
// logged and the loop continues; the aggregate error names every track that failed.
// An interrupt stops the loop at the track it cut short.
func forEachTrack(ctx context.Context, w io.Writer, in trackctx.Inputs, fn func(*trackctx.Track) error) error {
	if !trackctx.IsPattern(in.TrackArg) {
		tr, err := trackctx.Resolve(in)
		if err != nil {
			return err
		}
		return fn(tr)
	}

	if in.SpecPath != "" || in.PoolPath != "" || in.OutputPath != "" {
		return fmt.Errorf("--spec/--pool/--output cannot be combined with a glob pattern %q", in.TrackArg)
	}
	tracks, err := trackctx.ResolveGlob(in.TrackArg, in.TrackRoot)
	if err != nil {
		return err
	}
	if len(tracks) == 1 {
		return fn(tracks[0])
	}

	var failed []string
	for _, tr := range tracks {
		fmt.Fprintf(w, "\n%s %s\n", cBold.Sprint("━━"), cBold.Sprint(tr.Name()))
		if err := fn(tr); err != nil {
			if ctx.Err() != nil {
				return err
			}
			printWarn(w, fmt.Sprintf("%s failed: %v", tr.Name(), err))
			failed = append(failed, tr.Name())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d track(s) failed: %s", len(failed), len(tracks), strings.Join(failed, ", "))
	}
	return nil
}
