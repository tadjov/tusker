package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/api/router"
	"github.com/tadjov/tusker/internal/api/server"
	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/factory"
	"github.com/tadjov/tusker/internal/storage/pg"
	"github.com/tadjov/tusker/internal/storage/storagetest"
	"github.com/tadjov/tusker/internal/types/document"
	dquery "github.com/tadjov/tusker/internal/types/query"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

// newPostgresSearchServer serves the search API from a Postgres container
// loaded with storagetest.ParityCorpus.
func newPostgresSearchServer(t *testing.T) *server.Server {
	t.Helper()
	ctx := context.Background()
	container := pkgtesting.NewPGContainerWithCleanup(ctx, t)

	loader, err := pg.NewConnectionPool(ctx, pg.PoolConfig{ConnStr: container.ConnString})
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}
	defer loader.Close()
	indexer, err := pg.NewIndexer(loader)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	if err := indexer.SaveBulk(ctx, storagetest.ParityCorpus()); err != nil {
		t.Fatalf("SaveBulk: %v", err)
	}

	backend, err := openSearchBackend(ctx, &NewsSearchConfig{
		StorageConfig:   factory.StorageConfig{Type: storage.PG, Pg: &pg.PoolConfig{ConnStr: container.ConnString}},
		EmbeddingConfig: embedding.Config{Enabled: false},
	})
	if err != nil {
		t.Fatalf("openSearchBackend: %v", err)
	}
	t.Cleanup(backend.close)

	s, err := server.New(&server.Config{Port: "0"}, backend.health)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	s.SetupValidator().SetupErrorHandler()
	router.NewSearchRouter(s.Echo, backend.fts).Bind()
	return s
}

// articlesWhere returns the ids of the corpus articles that hold text in field.
func articlesWhere(field func(document.Article) string, text string) []uuid.UUID {
	var ids []uuid.UUID
	for _, article := range storagetest.ParityCorpus() {
		if strings.Contains(field(article), text) {
			ids = append(ids, article.ID)
		}
	}
	return ids
}

func contractText(a document.Article) string {
	return strings.Join([]string{a.Title, a.Description, a.Content}, " ")
}

func TestLenientSearchRunsWithoutWhatItIgnored(t *testing.T) {
	s := newPostgresSearchServer(t)

	tests := []struct {
		name        string
		body        string
		wantHits    []uuid.UUID
		wantFields  []dquery.Field
		wantIgnored []string
	}{
		{
			name:        "every searchable field is honoured",
			body:        `{"query":{"multi_match":{"fields":["title","description^2","content"],"query":"budget"}}}`,
			wantHits:    articlesWhere(contractText, "budget"),
			wantFields:  dquery.SearchableFields(),
			wantIgnored: nil,
		},
		{
			name:        "fuzziness is dropped",
			body:        `{"query":{"match":{"field":"title","query":"budgt","fuzziness":"auto"}}}`,
			wantHits:    nil,
			wantFields:  []dquery.Field{dquery.FieldTitle},
			wantIgnored: []string{"fuzziness"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/articles/_search", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Prefer", "handling=lenient")
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}

			var res struct {
				Hits []struct {
					Article struct {
						ID uuid.UUID `json:"id"`
					} `json:"article"`
				} `json:"hits"`
				Effective dquery.Effective `json:"effective"`
				Ignored   []dquery.Ignored `json:"ignored"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			var hits []uuid.UUID
			for _, hit := range res.Hits {
				hits = append(hits, hit.Article.ID)
			}
			if !slices.Equal(sortedIDs(hits), sortedIDs(tt.wantHits)) {
				t.Errorf("hits = %v, want %v", sortedIDs(hits), sortedIDs(tt.wantHits))
			}
			if !slices.Equal(res.Effective.Fields, tt.wantFields) {
				t.Errorf("effective fields = %v, want %v", res.Effective.Fields, tt.wantFields)
			}
			var ignored []string
			for _, ig := range res.Ignored {
				ignored = append(ignored, ig.Param)
			}
			slices.Sort(ignored)
			if !slices.Equal(ignored, tt.wantIgnored) {
				t.Errorf("ignored = %v, want %v", ignored, tt.wantIgnored)
			}
		})
	}
}

func TestSearchRejectsAFieldOutsideTheSearchableSet(t *testing.T) {
	s := newPostgresSearchServer(t)

	tests := []struct {
		name string
		body string
	}{
		{name: "match on author", body: `{"query":{"match":{"field":"author","query":"budget"}}}`},
		{name: "phrase on subtitle", body: `{"query":{"phrase":{"fields":["title","subtitle"],"query":"budget tariff"}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/articles/_search", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Prefer", "handling=lenient")
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 even under lenient handling (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func sortedIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return out
}
