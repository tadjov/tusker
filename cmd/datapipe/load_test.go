package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/parquet-go/parquet-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/datapackage"
	"github.com/tadjov/tusker/internal/ingest"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/pg"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

const loadFixtureModelTag = "qwen3-embedding:0.6b@1024"

var loadFixtureNamespace = uuid.MustParse("5a0c2f3e-8d41-4b6a-9e2f-1c7b3d9a6e54")

// producerRow writes pyarrow's 3-level LIST encoding.
type producerRow struct {
	ID        string    `parquet:"id"`
	Embedding []float32 `parquet:"embedding,list"`
}

func TestLoad(t *testing.T) {
	for _, engine := range []storage.Engine{storage.PostgresNative, storage.ParadeDB, storage.Tiger} {
		t.Run(string(engine), func(t *testing.T) {
			server := pkgtesting.NewPGContainerWithConfig(context.Background(), t, pkgtesting.PGConfig{
				Engine:     engine,
				Unmigrated: true,
			})

			t.Run("loads a canonical package and its embeddings and writes the record", func(t *testing.T) {
				db := newLoadDatabase(t, server.ConnString)
				root := t.TempDir()
				articles := loadFixtureArticles(2_000)
				canonical := writeCanonicalPackage(t, root, articles)
				embeddings := writeEmbeddingsPackage(t, root, canonical, articles, nil)

				err := execDatapipe(t, db, "load", "--engine", string(engine),
					"--canonical", canonical.location, "--embeddings", embeddings.location, "--batch-size", "500")
				require.NoError(t, err)

				stored := readLoadedArticles(t, db, articles[0].ID, articles[1_999].ID)
				assert.Equal(t, articles[0], stored[articles[0].ID])
				assert.Equal(t, articles[1_999], stored[articles[1_999].ID])
				assert.Contains(t, loadedIndexNames(t, db), "idx_article_embedding")

				written := writtenLoadRecords(t, filepath.Join(root, "datasets", "fixture", "loads", string(engine)))
				require.Len(t, written, 1)
				fromDatabase := corpusLoadRecord(t, db, written[0].FinishedAt)
				assert.Equal(t, fromDatabase, written[0])
				assert.Equal(t, storage.PackageRef{Name: "fixture-canonical", Version: "article-v2-test", ResourceHash: canonical.resourceHash}, written[0].Canonical)
				assert.Equal(t, &storage.PackageRef{Name: "fixture-embeddings", Version: "lead-v1-1024-test", ResourceHash: embeddings.resourceHash}, written[0].Embeddings)
				assert.Equal(t, loadFixtureModelTag, written[0].ModelTag)
				assert.Equal(t, int64(2_000), written[0].Vectors)
			})

			t.Run("loads a JSONL canonical package", func(t *testing.T) {
				db := newLoadDatabase(t, server.ConnString)
				root := t.TempDir()
				articles := loadFixtureArticles(20)
				articles[4].Metadata.Extra = map[string]any{"wordCount": float64(412), "paywalled": true}
				canonical := writeCanonicalPackageAs(t, root, articles, "jsonl")
				embeddings := writeEmbeddingsPackage(t, root, canonical, articles, nil)

				err := execDatapipe(t, db, "load", "--engine", string(engine),
					"--canonical", canonical.location, "--embeddings", embeddings.location)
				require.NoError(t, err)

				stored := readLoadedArticles(t, db, articles[4].ID, articles[19].ID)
				assert.Equal(t, articles[4], stored[articles[4].ID])
				assert.Equal(t, articles[19], stored[articles[19].ID])
			})

			t.Run("refuses embeddings built from another canonical package before touching the database", func(t *testing.T) {
				db := newLoadDatabase(t, server.ConnString)
				root := t.TempDir()
				articles := loadFixtureArticles(20)
				canonical := writeCanonicalPackage(t, root, articles)
				other := canonical
				other.resourceHash = "sha256:" + fmt.Sprintf("%064d", 7)
				embeddings := writeEmbeddingsPackage(t, root, other, articles, nil)

				err := execDatapipe(t, db, "load", "--engine", string(engine),
					"--canonical", canonical.location, "--embeddings", embeddings.location)

				require.ErrorContains(t, err, other.resourceHash)
				require.ErrorContains(t, err, canonical.resourceHash)
				assert.False(t, loadedTableExists(t, db, "articles"))
			})

			t.Run("loads a trial embeddings package only when asked to", func(t *testing.T) {
				db := newLoadDatabase(t, server.ConnString)
				root := t.TempDir()
				articles := loadFixtureArticles(20)
				canonical := writeCanonicalPackage(t, root, articles)
				limit := 5
				trial := writeEmbeddingsPackage(t, root, canonical, articles[:limit], &limit)
				args := []string{"load", "--engine", string(engine), "--canonical", canonical.location, "--embeddings", trial.location}

				err := execDatapipe(t, db, args...)
				require.ErrorContains(t, err, "--trial")
				assert.False(t, loadedTableExists(t, db, "articles"))

				require.NoError(t, execDatapipe(t, db, append(args, "--trial")...))
				written := writtenLoadRecords(t, filepath.Join(root, "datasets", "fixture", "loads", string(engine)))
				require.Len(t, written, 1)
				assert.Equal(t, int64(20), written[0].Articles)
				assert.Equal(t, int64(limit), written[0].Vectors)
			})

			t.Run("refuses embeddings for only some articles before touching the database", func(t *testing.T) {
				for _, format := range []string{"parquet", "jsonl"} {
					t.Run(format, func(t *testing.T) {
						db := newLoadDatabase(t, server.ConnString)
						root := t.TempDir()
						articles := loadFixtureArticles(20)
						canonical := writeCanonicalPackageAs(t, root, articles, format)
						short := writeEmbeddingsPackage(t, root, canonical, articles[:5], nil)

						err := execDatapipe(t, db, "load", "--engine", string(engine),
							"--canonical", canonical.location, "--embeddings", short.location)

						require.ErrorContains(t, err, "5 vectors for 20 articles")
						assert.False(t, loadedTableExists(t, db, "articles"))
					})
				}
			})

			t.Run("skip-index loads the rows without indexes or a record", func(t *testing.T) {
				db := newLoadDatabase(t, server.ConnString)
				root := t.TempDir()
				articles := loadFixtureArticles(20)
				canonical := writeCanonicalPackage(t, root, articles)

				err := execDatapipe(t, db, "load", "--engine", string(engine), "--canonical", canonical.location, "--skip-index")
				require.NoError(t, err)

				assert.Equal(t, articles[3], readLoadedArticles(t, db, articles[3].ID)[articles[3].ID])
				assert.NotContains(t, loadedIndexNames(t, db), "idx_article_embedding")
				assert.NoDirExists(t, filepath.Join(root, "datasets", "fixture", "loads"))
			})
		})
	}
}

func TestLoad_RefusesBeforeReachingAnEngine(t *testing.T) {
	t.Setenv("ES_ADDRESSES", "http://127.0.0.1:1")
	t.Setenv("ES_INDEX_NAME", "articles")

	t.Run("a canonical resource whose bytes changed", func(t *testing.T) {
		root := t.TempDir()
		canonical := writeCanonicalPackage(t, root, loadFixtureArticles(20))
		corruptByte(t, canonical.resourcePath)

		err := execDatapipe(t, "", "load", "--engine", "es", "--canonical", canonical.location)

		var mismatch *datapackage.HashMismatchError
		require.ErrorAs(t, err, &mismatch)
	})

	t.Run("a trial embeddings package without --trial, on every engine", func(t *testing.T) {
		root := t.TempDir()
		articles := loadFixtureArticles(20)
		canonical := writeCanonicalPackage(t, root, articles)
		limit := 5
		trial := writeEmbeddingsPackage(t, root, canonical, articles[:limit], &limit)

		err := execDatapipe(t, "", "load", "--engine", "es", "--canonical", canonical.location, "--embeddings", trial.location)

		require.ErrorContains(t, err, "--trial")
	})

	t.Run("embeddings for only some articles, on every engine", func(t *testing.T) {
		root := t.TempDir()
		articles := loadFixtureArticles(20)
		canonical := writeCanonicalPackage(t, root, articles)
		short := writeEmbeddingsPackage(t, root, canonical, articles[:5], nil)

		err := execDatapipe(t, "", "load", "--engine", "es", "--canonical", canonical.location, "--embeddings", short.location)

		require.ErrorContains(t, err, "5 vectors for 20 articles")
	})
}

func execDatapipe(t *testing.T, connString string, args ...string) error {
	t.Helper()
	t.Setenv("PG_CONNECTION_STRING", connString)
	cmd := newRootCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	return cmd.ExecuteContext(context.Background())
}

type fixturePackage struct {
	location     string
	resourcePath string
	resourceHash string
}

func writeCanonicalPackage(t *testing.T, root string, articles []document.Article) fixturePackage {
	t.Helper()
	return writeCanonicalPackageAs(t, root, articles, "parquet")
}

func writeCanonicalPackageAs(t *testing.T, root string, articles []document.Article, format string) fixturePackage {
	t.Helper()
	dir := filepath.Join(root, "datasets", "fixture", "canonical", "article-v2-test")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "articles."+format)
	f, err := os.Create(path)
	require.NoError(t, err)
	var w ingest.CanonicalWriter = ingest.NewParquetCanonicalWriter(f)
	if format == "jsonl" {
		w = ingest.NewJsonlCanonicalWriter(f)
	}
	require.NoError(t, w.Write(articles))
	require.NoError(t, w.Close())
	require.NoError(t, f.Close())

	pkg, err := datapackage.Write(context.Background(), dir, datapackage.Package{
		Name:          "fixture-canonical",
		Version:       "article-v2-test",
		ArticleSchema: datapackage.ArticleSchema,
		Language:      language.English,
		Resources:     []datapackage.Resource{{Name: datapackage.ArticlesResource, Path: "articles." + format, Format: format}},
	})
	require.NoError(t, err)
	return fixturePackage{location: filepath.Join(dir, datapackage.DescriptorName), resourcePath: path, resourceHash: pkg.Resources[0].Hash}
}

func writeEmbeddingsPackage(t *testing.T, root string, canonical fixturePackage, articles []document.Article, limit *int) fixturePackage {
	t.Helper()
	version := "lead-v1-1024-test"
	if limit != nil {
		version += fmt.Sprintf("-limit-%d", *limit)
	}
	dir := filepath.Join(root, "datasets", "fixture", "embeddings", "qwen3-embedding-0.6b@1024", version)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	rows := make([]producerRow, len(articles))
	for i, a := range articles {
		values := make([]float32, 1024)
		values[i%1024] = 1
		rows[i] = producerRow{ID: a.ID.String(), Embedding: values}
	}
	path := filepath.Join(dir, "embeddings.parquet")
	f, err := os.Create(path)
	require.NoError(t, err)
	w := parquet.NewGenericWriter[producerRow](f)
	_, err = w.Write(rows)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, f.Close())

	pkg, err := datapackage.Write(context.Background(), dir, datapackage.Package{
		Name:    "fixture-embeddings",
		Version: version,
		Embedding: &datapackage.Embedding{
			Model: "qwen3-embedding:0.6b", HuggingFaceModel: "Qwen/Qwen3-Embedding-0.6B", Revision: "abc123",
			Tag: loadFixtureModelTag, Dim: 1024, Recipe: "lead-v1", Pooling: "last_token", Normalized: true,
			MaxTokens: 512, BatchSize: 32, DType: "float16", Limit: limit,
		},
		Canonical: &datapackage.Ref{Name: "fixture-canonical", Version: "article-v2-test", ResourceHash: canonical.resourceHash},
		Resources: []datapackage.Resource{{Name: datapackage.EmbeddingsResource, Path: "embeddings.parquet", Format: "parquet"}},
	})
	require.NoError(t, err)
	return fixturePackage{location: filepath.Join(dir, datapackage.DescriptorName), resourcePath: path, resourceHash: pkg.Resources[0].Hash}
}

func corruptByte(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[len(data)/2] ^= 0xff
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

func loadFixtureArticles(n int) []document.Article {
	articles := make([]document.Article, n)
	for i := range n {
		articles[i] = document.Article{
			ID:              uuid.NewSHA1(loadFixtureNamespace, fmt.Appendf(nil, "article-%d", i)),
			URL:             fmt.Sprintf("https://news.example.com/%d", i),
			Title:           fmt.Sprintf("Storm number %d hits the coast", i),
			Description:     "Thousands without power",
			Content:         fmt.Sprintf("The storm made landfall overnight, report %d.", i),
			PublishedAt:     time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC).Add(time.Duration(i) * time.Hour),
			Language:        language.English,
			PublisherDomain: "example.com",
			Metadata:        document.Metadata{PublisherName: "Example News", Authors: []string{"Jane Doe"}},
		}
	}
	return articles
}

func newLoadDatabase(t *testing.T, serverConnString string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, serverConnString)
	require.NoError(t, err)
	defer conn.Close(ctx)
	name := "load_" + uuid.NewString()[:8]
	_, err = conn.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	config, err := pgx.ParseConfig(serverConnString)
	require.NoError(t, err)
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", config.User, config.Password, config.Host, config.Port, name)
}

func readLoadedArticles(t *testing.T, connString string, ids ...uuid.UUID) map[uuid.UUID]document.Article {
	t.Helper()
	pool, err := pg.NewConnectionPool(context.Background(), pg.PoolConfig{ConnStr: connString})
	require.NoError(t, err)
	defer pool.Close()
	articles, err := pg.NewArticleReader(pool).GetByIDs(context.Background(), ids)
	require.NoError(t, err)
	byID := make(map[uuid.UUID]document.Article, len(articles))
	for _, a := range articles {
		a.PublishedAt = a.PublishedAt.UTC()
		byID[a.ID] = a
	}
	return byID
}

func withLoadConn(t *testing.T, connString string, run func(*pgx.Conn) error) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), connString)
	require.NoError(t, err)
	defer conn.Close(context.Background())
	require.NoError(t, run(conn))
}

func loadedTableExists(t *testing.T, connString, table string) bool {
	t.Helper()
	var exists bool
	withLoadConn(t, connString, func(conn *pgx.Conn) error {
		return conn.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists)
	})
	return exists
}

func loadedIndexNames(t *testing.T, connString string) []string {
	t.Helper()
	var names []string
	withLoadConn(t, connString, func(conn *pgx.Conn) error {
		rows, err := conn.Query(context.Background(),
			"SELECT indexname FROM pg_indexes WHERE tablename IN ('articles', 'article_embeddings')")
		if err != nil {
			return err
		}
		names, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return names
}

func corpusLoadRecord(t *testing.T, connString string, finishedAt time.Time) storage.LoadRecord {
	t.Helper()
	var record storage.LoadRecord
	withLoadConn(t, connString, func(conn *pgx.Conn) error {
		return conn.QueryRow(context.Background(),
			"SELECT record FROM corpus_load WHERE finished_at = $1", finishedAt).Scan(&record)
	})
	return record
}

func writtenLoadRecords(t *testing.T, dir string) []storage.LoadRecord {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	records := make([]storage.LoadRecord, len(paths))
	for i, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &records[i]))
	}
	return records
}
