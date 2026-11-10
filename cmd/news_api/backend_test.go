package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tadjov/tusker/internal/api/router"
	"github.com/tadjov/tusker/internal/api/server"
	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/es"
	"github.com/tadjov/tusker/internal/storage/factory"
	"github.com/tadjov/tusker/internal/storage/pg"
	dquery "github.com/tadjov/tusker/internal/types/query"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

func pgSearchConfig(connStr string) *NewsSearchConfig {
	return &NewsSearchConfig{
		StorageConfig: factory.StorageConfig{
			Type: storage.PG,
			Pg:   &pg.PoolConfig{ConnStr: connStr},
		},
		EmbeddingConfig: embedding.Config{Enabled: true, BaseURL: "http://127.0.0.1:1"},
	}
}

func healthStatus(t *testing.T, backend *searchBackend) int {
	t.Helper()
	s, err := server.New(&server.Config{Port: "0"}, backend.health)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	s.SetupHealthChecks("/health")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code
}

func TestOpenSearchBackend_PostgresSearchersShareOnePool(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewPGContainerWithCleanup(ctx, t)

	backend, err := openSearchBackend(ctx, pgSearchConfig(container.ConnString+"&pool_max_conns=1"))
	if err != nil {
		t.Fatalf("openSearchBackend: %v", err)
	}
	t.Cleanup(backend.close)
	if backend.semantic == nil || backend.hybrid == nil {
		t.Fatalf("embeddings enabled but semantic=%v hybrid=%v", backend.semantic, backend.hybrid)
	}

	probe, err := pgx.Connect(ctx, container.ConnString)
	if err != nil {
		t.Fatalf("connect probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Close(ctx) })

	var connections int
	err = probe.QueryRow(ctx, `
		SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database()
		  AND backend_type = 'client backend'
		  AND pid <> pg_backend_pid()`).Scan(&connections)
	if err != nil {
		t.Fatalf("count connections: %v", err)
	}

	if connections > 1 {
		t.Errorf("API holds %d connections with pool_max_conns=1; searchers do not share one pool", connections)
	}
}

func TestOpenSearchBackend_HealthFollowsPostgres(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewPGContainerWithCleanup(ctx, t)

	running, err := openSearchBackend(ctx, pgSearchConfig(container.ConnString))
	if err != nil {
		t.Fatalf("openSearchBackend: %v", err)
	}
	t.Cleanup(running.close)
	closed, err := openSearchBackend(ctx, pgSearchConfig(container.ConnString))
	if err != nil {
		t.Fatalf("openSearchBackend: %v", err)
	}

	if got := healthStatus(t, running); got != http.StatusOK {
		t.Fatalf("/health with Postgres up = %d, want %d", got, http.StatusOK)
	}

	closed.close()
	if got := healthStatus(t, closed); got == http.StatusOK {
		t.Errorf("/health after the backend closed = %d, want non-200", got)
	}

	stopTimeout := 5 * time.Second
	if err := container.Container.Stop(ctx, &stopTimeout); err != nil {
		t.Fatalf("stop postgres: %v", err)
	}
	if got := healthStatus(t, running); got == http.StatusOK {
		t.Errorf("/health with Postgres stopped = %d, want non-200", got)
	}
}

func TestOpenSearchBackend_HealthFollowsElasticsearch(t *testing.T) {
	reachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(reachable.Close)

	tests := []struct {
		name    string
		address string
		healthy bool
	}{
		{name: "cluster answers", address: reachable.URL, healthy: true},
		{name: "nothing listening", address: unusedAddress(t), healthy: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &NewsSearchConfig{StorageConfig: factory.StorageConfig{
				Type: storage.ES,
				Es:   &es.ClientConfig{Addresses: []string{tt.address}, IndexName: "articles"},
			}}
			backend, err := openSearchBackend(context.Background(), cfg)
			if err != nil {
				t.Fatalf("openSearchBackend: %v", err)
			}
			t.Cleanup(backend.close)

			got := healthStatus(t, backend)
			if tt.healthy && got != http.StatusOK {
				t.Errorf("/health = %d, want %d", got, http.StatusOK)
			}
			if !tt.healthy && got == http.StatusOK {
				t.Errorf("/health = %d, want non-200", got)
			}
		})
	}
}

func unusedAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := "http://" + ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return address
}

// fakeElasticsearch answers every request as an Elasticsearch cluster with no
// matching documents, and reports how many connections it has accepted.
func fakeElasticsearch(t *testing.T) (url string, connections func() int64) {
	t.Helper()
	var accepted atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, `{"took":1,"timed_out":false,"_shards":{"total":1,"successful":1,"skipped":0,"failed":0},"hits":{"total":{"value":0,"relation":"eq"},"max_score":null,"hits":[]}}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			accepted.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL, accepted.Load
}

func fakeOllama(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		unit := make([]float32, embedding.DefaultDim)
		unit[0] = 1
		_ = json.NewEncoder(w).Encode(embedding.Response{Embedding: unit})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestOpenSearchBackend_ElasticsearchSearchersShareOneClient(t *testing.T) {
	esURL, connections := fakeElasticsearch(t)
	cfg := &NewsSearchConfig{
		StorageConfig: factory.StorageConfig{
			Type: storage.ES,
			Es:   &es.ClientConfig{Addresses: []string{esURL}, IndexName: "articles"},
		},
		EmbeddingConfig: embedding.Config{Enabled: true, BaseURL: fakeOllama(t)},
	}
	backend, err := openSearchBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("openSearchBackend: %v", err)
	}
	t.Cleanup(backend.close)
	if backend.semantic == nil || backend.hybrid == nil {
		t.Fatalf("embeddings enabled but semantic=%v hybrid=%v", backend.semantic, backend.hybrid)
	}

	ctx := context.Background()
	opts := &dquery.BaseOptions{Size: 10}
	if !backend.health.Healthy(ctx) {
		t.Fatal("health check failed against the fake cluster")
	}
	if _, err := backend.fts.SearchStringQuery(ctx, dquery.NewQueryString("climate"), opts); err != nil {
		t.Fatalf("full-text search: %v", err)
	}
	if _, err := backend.semantic.SearchSemantic(ctx, dquery.NewSemantic("climate"), opts); err != nil {
		t.Fatalf("semantic search: %v", err)
	}
	if _, err := backend.hybrid.SearchHybrid(ctx, dquery.NewHybrid("climate"), opts); err != nil {
		t.Fatalf("hybrid search: %v", err)
	}

	if got := connections(); got > 1 {
		t.Errorf("serial requests opened %d connections to Elasticsearch; searchers do not share one client", got)
	}
}

func TestServer_SearchPastRequestDeadlineReleasesItsConnection(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewPGContainerWithCleanup(ctx, t)
	backend, err := openSearchBackend(ctx, pgSearchConfig(container.ConnString+"&pool_max_conns=1"))
	if err != nil {
		t.Fatalf("openSearchBackend: %v", err)
	}
	t.Cleanup(backend.close)

	s, err := server.New(&server.Config{Port: "0", RequestTimeout: 200 * time.Millisecond}, backend.health)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	s.SetupMiddlewares().SetupErrorHandler().SetupHealthChecks("/health")
	router.NewSearchRouter(s.Echo, backend.fts).Bind()

	locker, err := pgx.Connect(ctx, container.ConnString)
	if err != nil {
		t.Fatalf("connect locker: %v", err)
	}
	t.Cleanup(func() { _ = locker.Close(ctx) })
	lock, err := locker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = lock.Rollback(ctx) })
	if _, err := lock.Exec(ctx, "LOCK TABLE articles IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock articles: %v", err)
	}

	search := httptest.NewRecorder()
	answered := make(chan struct{})
	go func() {
		s.ServeHTTP(search, httptest.NewRequest(http.MethodGet, "/v1/articles/search?q=climate", nil))
		close(answered)
	}()
	// The guard only stops a search that ignores its deadline from hanging the test.
	select {
	case <-answered:
	case <-time.After(30 * time.Second):
		t.Fatal("search blocked on a locked table outlived its request deadline")
	}
	if search.Code == http.StatusOK {
		t.Fatalf("search on a locked table = %d, want an error", search.Code)
	}

	health := httptest.NewRecorder()
	s.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Errorf("/health after the cancelled search = %d, want %d: its connection was not released", health.Code, http.StatusOK)
	}
}
