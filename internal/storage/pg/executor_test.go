package pg

import (
	"context"
	"flag"
	"os"
	"testing"

	"github.com/tadjov/tusker/internal/storage"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
	"github.com/testcontainers/testcontainers-go"
)

var (
	testCtx      context.Context
	testPool     *ConnectionPool
	testExecutor *RawExecutor
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		// These are integration tests backed by a Postgres container.
		// Skip them in -short mode (used by CI) so a Docker daemon isn't required.
		os.Exit(0)
	}

	testCtx = context.Background()

	pg, err := pkgtesting.NewPGContainer(testCtx, pkgtesting.PGConfig{
		Database: "news_test_db",
		Username: "test",
		Password: "test",
	})
	if err != nil {
		panic(err)
	}
	defer testcontainers.TerminateContainer(pg.Container)

	testPool, err = NewConnectionPool(testCtx, PoolConfig{ConnStr: pg.ConnString})
	if err != nil {
		panic(err)
	}
	defer testPool.Close()

	testExecutor = NewRawExecutor(testPool)

	os.Exit(m.Run())
}

func truncateTable(t *testing.T) {
	t.Helper()
	_, err := testPool.GetConn().Exec(testCtx, "TRUNCATE TABLE articles CASCADE")
	if err != nil {
		t.Fatalf("failed to truncate table: %v", err)
	}
}

func TestNewRawExecutor(t *testing.T) {
	if testExecutor == nil {
		t.Fatal("expected non-nil executor")
	}
	if testExecutor.db == nil {
		t.Fatal("expected non-nil db field")
	}
}

func TestRawExecutor_Exec_SimpleQuery(t *testing.T) {
	truncateTable(t)
	defer truncateTable(t)

	_, err := testPool.GetConn().Exec(testCtx, `
		INSERT INTO articles (id, title, content, url, language)
		VALUES (gen_random_uuid(), $1, $2, $3, $4)
	`, "Test Article", "This is test content", "http://test.com", "eng")
	if err != nil {
		t.Fatalf("failed to insert test data: %v", err)
	}

	result, err := testExecutor.Exec(testCtx, "SELECT * FROM articles WHERE title @@ plainto_tsquery('english', 'test')", nil, nil)
	if err != nil {
		t.Fatalf("failed to execute query: %v", err)
	}

	if result.RowCount != 1 {
		t.Errorf("expected 1 hit, got %d", result.RowCount)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("expected 1 hit in results, got %d", len(result.Hits))
	}

	hit := result.Hits[0]
	if title, ok := hit["title"].(string); !ok || title != "Test Article" {
		t.Errorf("expected title 'Test Article', got %v", hit["title"])
	}
}

func TestRawExecutor_Exec_ParameterizedQuery(t *testing.T) {
	truncateTable(t)
	defer truncateTable(t)

	_, err := testPool.GetConn().Exec(testCtx, `
		INSERT INTO articles (id, title, content, url, language)
		VALUES (gen_random_uuid(), $1, $2, $3, $4), (gen_random_uuid(), $5, $6, $7, $8)
	`,
		"Climate Change", "Article about climate", "http://climate.com", "eng",
		"Technology News", "Article about tech", "http://tech.com", "eng",
	)
	if err != nil {
		t.Fatalf("failed to insert test data: %v", err)
	}

	result, err := testExecutor.Exec(testCtx, `
		SELECT * FROM articles WHERE title @@ to_tsquery('english', $1::text)
	`, []interface{}{"climate | change"}, nil)
	if err != nil {
		t.Fatalf("failed to execute query: %v", err)
	}

	if result.RowCount != 1 {
		t.Errorf("expected 1 hit, got %d", result.RowCount)
	}

	hit := result.Hits[0]
	if title, ok := hit["title"].(string); !ok || title != "Climate Change" {
		t.Errorf("expected title 'Climate Change', got %v", hit["title"])
	}
}

func TestRawExecutor_Exec_MultipleParameters(t *testing.T) {
	truncateTable(t)
	defer truncateTable(t)

	_, err := testPool.GetConn().Exec(testCtx, `
		INSERT INTO articles (id, title, content, url, language, publisher_domain)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5)
	`, "Test Article", "Content here", "http://test.com", "eng", "test.com")
	if err != nil {
		t.Fatalf("failed to insert test data: %v", err)
	}

	result, err := testExecutor.Exec(testCtx, `
		SELECT * FROM articles WHERE title = $1 AND publisher_domain = $2
	`, []interface{}{"Test Article", "test.com"}, nil)
	if err != nil {
		t.Fatalf("failed to execute query: %v", err)
	}

	if result.RowCount != 1 {
		t.Errorf("expected 1 hit, got %d", result.RowCount)
	}
}

func TestRawExecutor_Exec_EmptyResults(t *testing.T) {
	truncateTable(t)
	defer truncateTable(t)

	result, err := testExecutor.Exec(testCtx, `
		SELECT * FROM articles WHERE title = $1
	`, []interface{}{"NonExistent"}, nil)
	if err != nil {
		t.Fatalf("failed to execute query: %v", err)
	}

	if result.RowCount != 0 {
		t.Errorf("expected 0 hits, got %d", result.RowCount)
	}
	if len(result.Hits) != 0 {
		t.Errorf("expected 0 hits in results, got %d", len(result.Hits))
	}
}

func TestRawExecutor_Exec_WithTimeout(t *testing.T) {
	truncateTable(t)
	defer truncateTable(t)

	_, err := testPool.GetConn().Exec(testCtx, `
		INSERT INTO articles (id, title, content, url, language)
		VALUES (gen_random_uuid(), $1, $2, $3, $4)
	`, "Test Article", "Content", "http://test.com", "eng")
	if err != nil {
		t.Fatalf("failed to insert test data: %v", err)
	}

	opts := &storage.ExecOptions{TimeoutSeconds: 5}
	result, err := testExecutor.Exec(testCtx, `
		SELECT * FROM articles
	`, nil, opts)
	if err != nil {
		t.Fatalf("failed to execute query: %v", err)
	}

	if result.RowCount != 1 {
		t.Errorf("expected 1 hit, got %d", result.RowCount)
	}
}

func TestRawExecutor_Exec_MultipleRows(t *testing.T) {
	truncateTable(t)
	defer truncateTable(t)

	for i := 0; i < 5; i++ {
		_, err := testPool.GetConn().Exec(testCtx, `
			INSERT INTO articles (id, title, content, url, language)
			VALUES (gen_random_uuid(), $1, $2, $3, $4)
		`, "Article Title", "Content", "http://test.com", "eng")
		if err != nil {
			t.Fatalf("failed to insert test data: %v", err)
		}
	}

	result, err := testExecutor.Exec(testCtx, `
		SELECT * FROM articles ORDER BY id
	`, nil, nil)
	if err != nil {
		t.Fatalf("failed to execute query: %v", err)
	}

	if result.RowCount != 5 {
		t.Errorf("expected 5 hits, got %d", result.RowCount)
	}
	if len(result.Hits) != 5 {
		t.Fatalf("expected 5 hits in results, got %d", len(result.Hits))
	}
}

func TestRawExecutor_Exec_AllFieldsReturned(t *testing.T) {
	truncateTable(t)
	defer truncateTable(t)

	_, err := testPool.GetConn().Exec(testCtx, `
		INSERT INTO articles (id, title, content, url, publisher_domain, metadata, language, description)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)
	`,
		"Full Test Article",
		"This is the full test content for the article",
		"http://full-test.com",
		"full-test.com",
		`{"key": "value", "number": 42}`,
		"eng",
		"Test description",
	)
	if err != nil {
		t.Fatalf("failed to insert test data: %v", err)
	}

	result, err := testExecutor.Exec(testCtx, `
		SELECT title, content, url, publisher_domain, metadata, language, description
		FROM articles
	`, nil, nil)
	if err != nil {
		t.Fatalf("failed to execute query: %v", err)
	}

	if result.RowCount != 1 {
		t.Fatalf("expected 1 hit, got %d", result.RowCount)
	}

	hit := result.Hits[0]

	expectedFields := map[string]interface{}{
		"title":            "Full Test Article",
		"content":          "This is the full test content for the article",
		"url":              "http://full-test.com",
		"publisher_domain": "full-test.com",
		"language":         "eng",
		"description":      "Test description",
	}

	for field, expectedValue := range expectedFields {
		actualValue, ok := hit[field]
		if !ok {
			t.Errorf("expected field %s to be present", field)
			continue
		}
		if actualValue != expectedValue {
			t.Errorf("field %s: expected %v, got %v", field, expectedValue, actualValue)
		}
	}

	if _, ok := hit["metadata"]; !ok {
		t.Error("expected metadata field to be present")
	}
}
