package factory

import (
	"testing"
	"time"

	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/es"
	"github.com/tadjov/tusker/internal/storage/pg"
)

func TestVectorSearchers_RequireEmbedder(t *testing.T) {
	esClient, err := es.NewClient(es.ClientConfig{Addresses: []string{"http://127.0.0.1:1"}, IndexName: "articles"})
	if err != nil {
		t.Fatalf("es.NewClient: %v", err)
	}
	backends := []struct {
		name string
		cfg  SearcherConfig
	}{
		{name: "postgres", cfg: SearcherConfig{Type: storage.PG, Pool: &pg.ConnectionPool{}}},
		{name: "elasticsearch", cfg: SearcherConfig{Type: storage.ES, EsClient: esClient}},
	}

	for _, backend := range backends {
		t.Run(backend.name+"/semantic", func(t *testing.T) {
			if _, err := NewSemanticSearcher(backend.cfg); err == nil {
				t.Error("NewSemanticSearcher without an embedder succeeded, want an error")
			}
		})
		t.Run(backend.name+"/hybrid", func(t *testing.T) {
			if _, err := NewHybridSearcher(backend.cfg); err == nil {
				t.Error("NewHybridSearcher without an embedder succeeded, want an error")
			}
		})
	}
}

func TestLoadEnv_ElasticsearchTimeouts(t *testing.T) {
	esEnv := func(t *testing.T) {
		t.Setenv("STORAGE_TYPE", string(storage.ES))
		t.Setenv("ES_ADDRESSES", "http://localhost:9200")
		t.Setenv("ES_INDEX_NAME", "articles")
	}

	t.Run("env overrides", func(t *testing.T) {
		esEnv(t)
		t.Setenv("ES_DIAL_TIMEOUT", "2s")
		t.Setenv("ES_RESPONSE_HEADER_TIMEOUT", "3m")

		cfg, err := LoadEnv()
		if err != nil {
			t.Fatalf("LoadEnv: %v", err)
		}
		if cfg.Es.DialTimeout != 2*time.Second {
			t.Errorf("DialTimeout = %v, want 2s", cfg.Es.DialTimeout)
		}
		if cfg.Es.ResponseHeaderTimeout != 3*time.Minute {
			t.Errorf("ResponseHeaderTimeout = %v, want 3m", cfg.Es.ResponseHeaderTimeout)
		}
	})

	t.Run("unset leaves the client defaults", func(t *testing.T) {
		esEnv(t)
		cfg, err := LoadEnv()
		if err != nil {
			t.Fatalf("LoadEnv: %v", err)
		}
		if cfg.Es.DialTimeout != 0 || cfg.Es.ResponseHeaderTimeout != 0 {
			t.Errorf("timeouts = %v, %v, want 0 so the client applies its defaults", cfg.Es.DialTimeout, cfg.Es.ResponseHeaderTimeout)
		}
	})

	for _, value := range []string{"30", "-1s", "0s"} {
		t.Run("rejects "+value, func(t *testing.T) {
			esEnv(t)
			t.Setenv("ES_RESPONSE_HEADER_TIMEOUT", value)
			if _, err := LoadEnv(); err == nil {
				t.Errorf("LoadEnv accepted ES_RESPONSE_HEADER_TIMEOUT=%q", value)
			}
		})
	}
}
