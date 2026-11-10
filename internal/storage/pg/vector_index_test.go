package pg

import (
	"strconv"
	"strings"
	"testing"

	"github.com/tadjov/tusker/internal/storage"
)

func TestArticleEmbeddingIndex_DeclaresHNSWBuildParams(t *testing.T) {
	var options []string
	err := testPool.GetConn().QueryRow(testCtx,
		`SELECT reloptions FROM pg_class WHERE relname = 'idx_article_embedding'`).Scan(&options)
	if err != nil {
		t.Fatalf("read reloptions of idx_article_embedding: %v", err)
	}

	declared := map[string]int{
		"m":               storage.HNSWM,
		"ef_construction": storage.HNSWEfConstruction,
	}
	for name, want := range declared {
		got, ok := reloption(options, name)
		if !ok {
			t.Errorf("index leaves %s to the pgvector default; reloptions = %v", name, options)
			continue
		}
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
}

func reloption(options []string, name string) (int, bool) {
	for _, opt := range options {
		key, value, found := strings.Cut(opt, "=")
		if !found || key != name {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, false
		}
		return parsed, true
	}
	return 0, false
}
