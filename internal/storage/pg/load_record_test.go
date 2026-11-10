package pg

import (
	"testing"

	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

// The records match what the Postgres loader stores in corpus_load.
const (
	olderLoadRecord = `{"engine": "pg-native", "engineVersion": "18.0",
	  "startedAt": "2026-09-01T10:00:00Z", "finishedAt": "2026-09-01T10:30:00Z",
	  "canonical": {"name": "cc-news", "version": "article-v2-3fafa1cf", "resourceHash": "sha256:1111111111111111111111111111111111111111111111111111111111111111"},
	  "embeddings": null, "modelTag": "", "articles": 2000, "vectors": 0, "batchSize": 5000,
	  "stages": [{"name": "copy_articles", "seconds": 1.5}], "settings": {}}`
	latestLoadRecord = `{"engine": "pg-native", "engineVersion": "18.0",
	  "startedAt": "2026-10-01T10:00:00Z", "finishedAt": "2026-10-01T10:30:00Z",
	  "canonical": {"name": "cc-news", "version": "article-v2-601d5476", "resourceHash": "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
	  "embeddings": {"name": "cc-news-embeddings", "version": "lead-v1-1024-938b2254", "resourceHash": "sha256:3333333333333333333333333333333333333333333333333333333333333333"},
	  "modelTag": "qwen3-embedding:0.6b@1024", "articles": 707910, "vectors": 707910, "batchSize": 5000,
	  "stages": [{"name": "copy_articles", "seconds": 120.5}, {"name": "index_hnsw", "seconds": 900}],
	  "settings": {"maintenance_work_mem": "2GB"}}`
)

func newLoadRecordTestPool(t *testing.T) *ConnectionPool {
	t.Helper()
	container := pkgtesting.NewPGContainerWithCleanup(testCtx, t)
	pool, err := NewConnectionPool(testCtx, PoolConfig{ConnStr: container.ConnString})
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestDescribeIndexes_ServerWithoutALoadRecordHasNoLoad(t *testing.T) {
	description, err := DescribeIndexes(testCtx, newLoadRecordTestPool(t))
	if err != nil {
		t.Fatalf("DescribeIndexes: %v", err)
	}

	if description.Load != nil {
		t.Errorf("load = %+v, want none on a server nothing was loaded into", description.Load)
	}
}

func createCorpusLoad(t *testing.T, pool *ConnectionPool) {
	t.Helper()
	if _, err := pool.GetConn().Exec(testCtx,
		`CREATE TABLE IF NOT EXISTS corpus_load (finished_at timestamptz PRIMARY KEY, record jsonb NOT NULL)`); err != nil {
		t.Fatalf("create corpus_load: %v", err)
	}
}

func TestDescribeIndexes_EmptyCorpusLoadHasNoLoad(t *testing.T) {
	pool := newLoadRecordTestPool(t)
	createCorpusLoad(t, pool)

	description, err := DescribeIndexes(testCtx, pool)
	if err != nil {
		t.Fatalf("DescribeIndexes: %v", err)
	}

	if description.Load != nil {
		t.Errorf("load = %+v, want none before the first load", description.Load)
	}
}

func TestDescribeIndexes_ReadsTheLatestLoadRecord(t *testing.T) {
	pool := newLoadRecordTestPool(t)
	createCorpusLoad(t, pool)
	for _, record := range []struct{ finishedAt, json string }{
		{finishedAt: "2026-10-01T10:30:00Z", json: latestLoadRecord},
		{finishedAt: "2026-09-01T10:30:00Z", json: olderLoadRecord},
	} {
		if _, err := pool.GetConn().Exec(testCtx,
			`INSERT INTO corpus_load (finished_at, record) VALUES ($1, $2)`, record.finishedAt, record.json); err != nil {
			t.Fatalf("insert load record: %v", err)
		}
	}

	description, err := DescribeIndexes(testCtx, pool)
	if err != nil {
		t.Fatalf("DescribeIndexes: %v", err)
	}

	load := description.Load
	if load == nil {
		t.Fatal("load not described")
	}
	if load.Canonical.Version != "article-v2-601d5476" {
		t.Errorf("canonical version = %q, want the latest load's article-v2-601d5476", load.Canonical.Version)
	}
	if load.Embeddings == nil || load.Embeddings.Version != "lead-v1-1024-938b2254" {
		t.Errorf("embeddings = %+v, want lead-v1-1024-938b2254", load.Embeddings)
	}
	if load.ModelTag != "qwen3-embedding:0.6b@1024" {
		t.Errorf("model tag = %q, want qwen3-embedding:0.6b@1024", load.ModelTag)
	}
	if load.Articles != 707910 || load.Vectors != 707910 {
		t.Errorf("articles, vectors = %d, %d, want 707910 each", load.Articles, load.Vectors)
	}
}
