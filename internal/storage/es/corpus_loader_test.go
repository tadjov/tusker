package es

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
	dquery "github.com/tadjov/tusker/internal/types/query"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

const loadTestModelTag = "qwen3-embedding:0.6b@1024"

func TestCorpusLoader_LoadCorpus(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewESContainer(ctx, t)

	t.Run("declares the contract's index settings and mapping", func(t *testing.T) {
		index := "articles_settings_test"
		articles := testArticles(3)
		if _, err := loadCorpus(t, container.Address, index, corpusOf(articles, nil), 100); err != nil {
			t.Fatalf("LoadCorpus: %v", err)
		}

		var settings map[string]struct {
			Settings map[string]any `json:"settings"`
		}
		getJSON(t, container.Address+"/"+index+"/_settings?flat_settings=true", &settings)
		got := settings[index].Settings
		for key, want := range map[string]string{
			"index.number_of_shards":             "1",
			"index.number_of_replicas":           "0",
			"index.max_result_window":            "10001",
			"index.refresh_interval":             "1s",
			"index.similarity.article_bm25.type": "BM25",
			"index.similarity.article_bm25.k1":   "1.2",
			"index.similarity.article_bm25.b":    "0.75",
		} {
			if got[key] != any(want) {
				t.Errorf("%s = %v, want %q", key, got[key], want)
			}
		}

		var mappings map[string]struct {
			Mappings struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"mappings"`
		}
		getJSON(t, container.Address+"/"+index+"/_mapping", &mappings)
		properties := mappings[index].Mappings.Properties
		want := map[string]string{
			"title":            `{"type":"text","analyzer":"english_analyzer","similarity":"article_bm25","fields":{"keyword":{"type":"keyword"}}}`,
			"description":      `{"type":"text","analyzer":"english_analyzer","similarity":"article_bm25"}`,
			"content":          `{"type":"text","analyzer":"english_analyzer","similarity":"article_bm25"}`,
			"id":               `{"type":"keyword"}`,
			"url":              `{"type":"keyword"}`,
			"language":         `{"type":"keyword"}`,
			"publisher_domain": `{"type":"keyword"}`,
			"embedding_model":  `{"type":"keyword"}`,
			"published_at":     `{"type":"date"}`,
			"metadata":         `{"type":"object","enabled":false}`,
			"embedding":        `{"type":"dense_vector","dims":1024,"index":true,"similarity":"cosine","index_options":{"type":"hnsw","m":16,"ef_construction":64}}`,
		}
		for field, mapping := range want {
			assertJSONEqual(t, field, properties[field], mapping)
		}
		for field := range properties {
			if _, ok := want[field]; !ok {
				t.Errorf("mapping has undeclared field %q", field)
			}
		}

		var analyzed struct {
			Tokens []struct {
				Token string `json:"token"`
			} `json:"tokens"`
		}
		postJSON(t, container.Address+"/"+index+"/_analyze", `{"field":"content","text":"The president's news about skies"}`, &analyzed)
		var tokens []string
		for _, token := range analyzed.Tokens {
			tokens = append(tokens, token.Token)
		}
		if want := []string{"presid", "news", "about", "sky"}; !slices.Equal(tokens, want) {
			t.Errorf("content analyzes %q to %q, want %q", "The president's news about skies", tokens, want)
		}
	})

	t.Run("stores every article with its own vector and model tag", func(t *testing.T) {
		index := "articles_loaded_test"
		articles := testArticles(2000)
		corpus := corpusOf(articles, testVectors(articles))
		corpus.Articles = refreshingMidway(t, container.Address, index, articles)
		record, err := loadCorpus(t, container.Address, index, corpus, 500)
		if err != nil {
			t.Fatalf("LoadCorpus: %v", err)
		}

		t.Run("merged into one segment", func(t *testing.T) {
			var segments []map[string]any
			getJSON(t, container.Address+"/_cat/segments/"+index+"?format=json", &segments)
			if len(segments) != 1 {
				t.Errorf("_cat/segments lists %d segments, want 1: %v", len(segments), segments)
			}
		})

		t.Run("stores the returned record under _meta.load", func(t *testing.T) {
			var mappings map[string]struct {
				Mappings struct {
					Meta struct {
						Load storage.LoadRecord `json:"load"`
					} `json:"_meta"`
				} `json:"mappings"`
			}
			getJSON(t, container.Address+"/"+index+"/_mapping", &mappings)
			if stored := mappings[index].Mappings.Meta.Load; !reflect.DeepEqual(stored, record) {
				t.Errorf("_meta.load = %+v, want the returned record %+v", stored, record)
			}
		})

		t.Run("records what was loaded and how", func(t *testing.T) {
			if record.Engine != "es" || record.EngineVersion != "8.12.0" {
				t.Errorf("engine = %q %q, want es 8.12.0", record.Engine, record.EngineVersion)
			}
			if record.Articles != 2000 || record.Vectors != 2000 {
				t.Errorf("articles, vectors = %d, %d, want 2000, 2000", record.Articles, record.Vectors)
			}
			if record.Canonical != corpus.Canonical || record.Embeddings == nil || *record.Embeddings != *corpus.Embeddings {
				t.Errorf("packages = %+v %+v, want %+v %+v", record.Canonical, record.Embeddings, corpus.Canonical, corpus.Embeddings)
			}
			if record.ModelTag != loadTestModelTag || record.BatchSize != 500 {
				t.Errorf("model tag, batch size = %q, %d, want %q, 500", record.ModelTag, record.BatchSize, loadTestModelTag)
			}
			for key, want := range map[string]string{
				"index.number_of_shards":                  "1",
				"index.number_of_replicas":                "0",
				"index.max_result_window":                 "10001",
				"index.refresh_interval":                  "1s",
				"embedding.index_options.type":            "hnsw",
				"embedding.index_options.m":               "16",
				"embedding.index_options.ef_construction": "64",
				"segments":               "1",
				"similarity.k1":          "1.2",
				"similarity.b":           "0.75",
				"title.similarity":       "article_bm25",
				"description.similarity": "article_bm25",
				"content.similarity":     "article_bm25",
			} {
				if got := record.Settings[key]; got != want {
					t.Errorf("settings[%s] = %q, want %q", key, got, want)
				}
			}
			if record.FinishedAt.Before(record.StartedAt) {
				t.Errorf("finished %v before it started %v", record.FinishedAt, record.StartedAt)
			}
			var stages []string
			for _, stage := range record.Stages {
				stages = append(stages, stage.Name)
			}
			if want := []string{"create_index", "bulk", "refresh", "force_merge"}; !slices.Equal(stages, want) {
				t.Errorf("stages = %q, want %q", stages, want)
			}
		})

		t.Run("search hits carry no embedding in _source", func(t *testing.T) {
			proxy, sources := recordingProxy(t, container.Address)
			searcher := NewSearcher(newTestClient(t, ClientConfig{Addresses: []string{proxy}, IndexName: index}))
			for _, method := range searchMethods {
				res, err := method.search(context.Background(), searcher, "headline", &dquery.BaseOptions{Size: 10})
				if err != nil {
					t.Fatalf("%s: %v", method.name, err)
				}
				if len(res.Hits) == 0 {
					t.Errorf("%s found nothing in the loaded index", method.name)
				}
			}
			if len(*sources) == 0 {
				t.Fatal("no search hit passed through the proxy")
			}
			for _, source := range *sources {
				if _, ok := source["embedding"]; ok {
					t.Errorf("hit %v carries its embedding in _source", source["id"])
				}
			}
		})

		stored := fetchByID(t, container.Address, index, articles)
		for i, a := range articles {
			doc, ok := stored[a.ID.String()]
			if !ok {
				t.Errorf("article %s not stored", a.ID)
				continue
			}
			want := storedDocument{
				ID:              a.ID.String(),
				URL:             a.URL,
				Title:           a.Title,
				Description:     a.Description,
				Content:         a.Content,
				PublishedAt:     a.PublishedAt.Format(time.RFC3339),
				Language:        "eng",
				PublisherDomain: a.PublisherDomain,
				Metadata:        map[string]any{"publisherName": "Example News", "authors": []any{"A. Writer"}},
				Embedding:       vectorFor(i),
				EmbeddingModel:  loadTestModelTag,
			}
			if diff := cmpDocuments(doc, want); diff != "" {
				t.Errorf("article %d (%s): %s", i, a.ID, diff)
			}
		}
	})

	for _, tc := range []struct {
		name    string
		index   string
		corrupt func(articles []document.Article, vecs []embedding.Vec)
	}{
		{
			name:    "a NaN vector",
			index:   "articles_nan_vector_test",
			corrupt: func(_ []document.Article, vecs []embedding.Vec) { vecs[3].Embedding[0] = float32(math.NaN()) },
		},
		{
			name:  "a publication date past year 9999",
			index: "articles_year_10000_test",
			corrupt: func(articles []document.Article, _ []embedding.Vec) {
				articles[3].PublishedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			},
		},
		{
			name:    "a vector elasticsearch rejects for its width",
			index:   "articles_short_vector_test",
			corrupt: func(_ []document.Article, vecs []embedding.Vec) { vecs[3].Embedding = vecs[3].Embedding[:3] },
		},
	} {
		t.Run(tc.name+" fails the load, names the article and leaves no index", func(t *testing.T) {
			index := tc.index
			articles := testArticles(5)
			vecs := testVectors(articles)
			tc.corrupt(articles, vecs)

			_, err := loadCorpus(t, container.Address, index, corpusOf(articles, vecs), 2)
			var failed *DocumentsFailedError
			if !errors.As(err, &failed) {
				t.Fatalf("LoadCorpus error = %v, want *DocumentsFailedError", err)
			}
			if failed.Failed != 1 || len(failed.Failures) != 1 || failed.Failures[0].ID != articles[3].ID {
				t.Errorf("failures = %d %+v, want only article %s", failed.Failed, failed.Failures, articles[3].ID)
			}
			if !strings.Contains(err.Error(), articles[3].ID.String()) {
				t.Errorf("error %q does not name article %s", err, articles[3].ID)
			}
			assertNoIndex(t, container.Address, index)
		})
	}

	for _, tc := range []struct {
		name    string
		index   string
		pairing func(vecs []embedding.Vec) []embedding.Vec
	}{
		{
			name:    "vectors out of article order",
			index:   "articles_swapped_vectors_test",
			pairing: func(vecs []embedding.Vec) []embedding.Vec { vecs[1], vecs[2] = vecs[2], vecs[1]; return vecs },
		},
		{
			name:  "more vectors than articles",
			index: "articles_extra_vector_test",
			pairing: func(vecs []embedding.Vec) []embedding.Vec {
				return append(vecs, embedding.Vec{ID: document.DeriveArticleID("https://example.com/extra"), Embedding: vectorFor(9)})
			},
		},
		{
			name:    "a vector tagged with another model",
			index:   "articles_other_model_test",
			pairing: func(vecs []embedding.Vec) []embedding.Vec { vecs[1].Model = "nomic-embed-text"; return vecs },
		},
		{
			name:    "an empty vector",
			index:   "articles_empty_vector_test",
			pairing: func(vecs []embedding.Vec) []embedding.Vec { vecs[1].Embedding = []float32{}; return vecs },
		},
		{
			name:    "a null vector",
			index:   "articles_null_vector_test",
			pairing: func(vecs []embedding.Vec) []embedding.Vec { vecs[1].Embedding = nil; return vecs },
		},
	} {
		t.Run(tc.name+": the load fails and leaves no index", func(t *testing.T) {
			articles := testArticles(4)
			if _, err := loadCorpus(t, container.Address, tc.index, corpusOf(articles, tc.pairing(testVectors(articles))), 2); err == nil {
				t.Fatal("LoadCorpus paired articles with vectors that are not theirs")
			}
			assertNoIndex(t, container.Address, tc.index)
		})
	}

	t.Run("stops on cancel and leaves no index", func(t *testing.T) {
		index := "articles_cancelled_test"
		articles := testArticles(10)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		corpus := corpusOf(articles, nil)
		corpus.Articles = func(yield func(document.Article, error) bool) {
			for i, a := range articles {
				if i == 5 {
					cancel()
				}
				if !yield(a, nil) {
					return
				}
			}
		}

		client := newTestClient(t, ClientConfig{Addresses: []string{container.Address}, IndexName: index})
		_, err := NewCorpusLoader(client).LoadCorpus(ctx, corpus, storage.LoadOptions{BatchSize: 2})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("LoadCorpus error = %v, want context.Canceled", err)
		}
		assertNoIndex(t, container.Address, index)
	})

	t.Run("a trial's vector prefix leaves the remaining articles without a vector", func(t *testing.T) {
		index := "articles_trial_prefix_test"
		articles := testArticles(6)
		record, err := loadCorpus(t, container.Address, index, corpusOf(articles, testVectors(articles)[:3]), 2)
		if err != nil {
			t.Fatalf("LoadCorpus: %v", err)
		}
		if record.Articles != 6 || record.Vectors != 3 {
			t.Errorf("articles, vectors = %d, %d, want 6, 3", record.Articles, record.Vectors)
		}
		stored := fetchByID(t, container.Address, index, articles)
		for i, a := range articles {
			doc := stored[a.ID.String()]
			if i < 3 && (!slices.Equal(doc.Embedding, vectorFor(i)) || doc.EmbeddingModel != loadTestModelTag) {
				t.Errorf("article %d lost its vector or tag: model %q", i, doc.EmbeddingModel)
			}
			if i >= 3 && (doc.Embedding != nil || doc.EmbeddingModel != "") {
				t.Errorf("article %d past the prefix carries a vector tagged %q", i, doc.EmbeddingModel)
			}
		}
	})

	t.Run("waits out a task that runs past the response-header timeout", func(t *testing.T) {
		var bulk strings.Builder
		for i := range 20 {
			fmt.Fprintf(&bulk, "{\"index\":{\"_index\":\"task_wait_source\"}}\n{\"n\":%d}\n", i)
		}
		postJSON(t, container.Address+"/_bulk?refresh=true", bulk.String(), &map[string]any{})
		var started struct {
			Task string `json:"task"`
		}
		// Throttled to one document a second, the reindex outlasts the 6s header timeout.
		postJSON(t, container.Address+"/_reindex?wait_for_completion=false&requests_per_second=1",
			`{"source":{"index":"task_wait_source","size":2},"dest":{"index":"task_wait_dest"}}`, &started)

		client := newTestClient(t, ClientConfig{Addresses: []string{container.Address}, IndexName: "task_wait_dest", ResponseHeaderTimeout: 6 * time.Second})
		if err := NewCorpusLoader(client).waitForTask(ctx, started.Task); err != nil {
			t.Errorf("waitForTask: %v", err)
		}
	})

	t.Run("keeps refresh off while the articles are written", func(t *testing.T) {
		index := "articles_refresh_off_test"
		articles := testArticles(4)
		var duringLoad any
		corpus := corpusOf(articles, nil)
		corpus.Articles = func(yield func(document.Article, error) bool) {
			for i, a := range articles {
				if i == 2 {
					var settings map[string]struct {
						Settings map[string]any `json:"settings"`
					}
					getJSON(t, container.Address+"/"+index+"/_settings?flat_settings=true", &settings)
					duringLoad = settings[index].Settings["index.refresh_interval"]
				}
				if !yield(a, nil) {
					return
				}
			}
		}
		if _, err := loadCorpus(t, container.Address, index, corpus, 2); err != nil {
			t.Fatalf("LoadCorpus: %v", err)
		}
		if duringLoad != any("-1") {
			t.Errorf("index.refresh_interval during the load = %v, want \"-1\"", duringLoad)
		}
	})

	t.Run("an article in another language than the corpus fails the load and leaves no index", func(t *testing.T) {
		index := "articles_mixed_language_test"
		articles := testArticles(4)
		articles[2].Language = "srp"
		_, err := loadCorpus(t, container.Address, index, corpusOf(articles, nil), 2)
		if err == nil {
			t.Fatal("LoadCorpus stored a Serbian article in an English corpus")
		}
		if !strings.Contains(err.Error(), articles[2].ID.String()) {
			t.Errorf("error %q does not name article %s", err, articles[2].ID)
		}
		assertNoIndex(t, container.Address, index)
	})

	t.Run("refuses a corpus language with no declared analyzer and creates no index", func(t *testing.T) {
		index := "articles_language_test"
		c := corpusOf(testArticles(1), nil)
		c.Language = "srp"
		if _, err := loadCorpus(t, container.Address, index, c, 100); err == nil {
			t.Fatal("LoadCorpus accepted a Serbian corpus into an English-only index")
		}
		assertNoIndex(t, container.Address, index)
	})

	t.Run("refuses an existing index and leaves its documents alone", func(t *testing.T) {
		index := "articles_existing_test"
		var created map[string]any
		sendJSON(t, http.MethodPut, container.Address+"/"+index+"/_doc/kept?refresh=true", `{"title":"kept"}`, &created)

		_, err := loadCorpus(t, container.Address, index, corpusOf(testArticles(2), nil), 100)
		var exists *IndexExistsError
		if !errors.As(err, &exists) {
			t.Fatalf("LoadCorpus error = %v, want *IndexExistsError", err)
		}
		var kept struct {
			Found bool `json:"found"`
		}
		getJSON(t, container.Address+"/"+index+"/_doc/kept", &kept)
		if !kept.Found {
			t.Error("the existing document is gone after a refused load")
		}
	})
}

type storedDocument struct {
	ID              string         `json:"id"`
	URL             string         `json:"url"`
	Title           string         `json:"title"`
	Description     string         `json:"description"`
	Content         string         `json:"content"`
	PublishedAt     string         `json:"published_at"`
	Language        string         `json:"language"`
	PublisherDomain string         `json:"publisher_domain"`
	Metadata        map[string]any `json:"metadata"`
	Embedding       []float32      `json:"embedding"`
	EmbeddingModel  string         `json:"embedding_model"`
}

func cmpDocuments(got, want storedDocument) string {
	if !slices.Equal(got.Embedding, want.Embedding) {
		return "carries another vector than its own"
	}
	got.Embedding, want.Embedding = nil, nil
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		return fmt.Sprintf("stored %s, want %s", gotJSON, wantJSON)
	}
	return ""
}

func fetchByID(t *testing.T, address, index string, articles []document.Article) map[string]storedDocument {
	t.Helper()
	ids := make([]string, len(articles))
	for i, a := range articles {
		ids[i] = a.ID.String()
	}
	body, err := json.Marshal(map[string]any{"ids": ids})
	if err != nil {
		t.Fatalf("marshal ids: %v", err)
	}
	var res struct {
		Docs []struct {
			ID     string         `json:"_id"`
			Found  bool           `json:"found"`
			Source storedDocument `json:"_source"`
		} `json:"docs"`
	}
	postJSON(t, address+"/"+index+"/_mget", string(body), &res)
	stored := make(map[string]storedDocument, len(res.Docs))
	for _, doc := range res.Docs {
		if doc.Found {
			stored[doc.ID] = doc.Source
		}
	}
	return stored
}

func recordingProxy(t *testing.T, address string) (string, *[]map[string]any) {
	t.Helper()
	target, err := url.Parse(address)
	if err != nil {
		t.Fatalf("parse %s: %v", address, err)
	}
	var sources []map[string]any
	proxy := httputil.NewSingleHostReverseProxy(target)
	forward := proxy.Director
	proxy.Director = func(req *http.Request) {
		forward(req)
		// The proxy cannot read a gzipped body.
		req.Header.Del("Accept-Encoding")
	}
	proxy.ModifyResponse = func(res *http.Response) error {
		if !strings.HasSuffix(res.Request.URL.Path, "/_search") {
			return nil
		}
		body, err := io.ReadAll(res.Body)
		if err != nil {
			return err
		}
		_ = res.Body.Close()
		res.Body = io.NopCloser(bytes.NewReader(body))
		var page struct {
			Hits struct {
				Hits []struct {
					Source map[string]any `json:"_source"`
				} `json:"hits"`
			} `json:"hits"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		for _, hit := range page.Hits.Hits {
			sources = append(sources, hit.Source)
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return server.URL, &sources
}

func assertNoIndex(t *testing.T, address, index string) {
	t.Helper()
	res, err := http.Head(address + "/" + index)
	if err != nil {
		t.Fatalf("HEAD %s: %v", index, err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("index %s exists (status %d), want none left behind", index, res.StatusCode)
	}
}

func loadCorpus(t *testing.T, address, index string, c storage.Corpus, batchSize int) (storage.LoadRecord, error) {
	t.Helper()
	client := newTestClient(t, ClientConfig{Addresses: []string{address}, IndexName: index})
	return NewCorpusLoader(client).LoadCorpus(context.Background(), c, storage.LoadOptions{BatchSize: batchSize})
}

func testArticles(n int) []document.Article {
	articles := make([]document.Article, n)
	for i := range articles {
		url := fmt.Sprintf("https://example.com/news/%d", i)
		articles[i] = document.Article{
			ID:              document.DeriveArticleID(url),
			URL:             url,
			Title:           fmt.Sprintf("Headline number %d", i),
			Description:     fmt.Sprintf("Lead paragraph %d", i),
			Content:         fmt.Sprintf("Body text of article %d", i),
			PublishedAt:     time.Date(2024, 1, 1+i%28, 12, 0, 0, 0, time.UTC),
			Language:        language.English,
			PublisherDomain: "example.com",
			Metadata:        document.Metadata{PublisherName: "Example News", Authors: []string{"A. Writer"}},
		}
	}
	return articles
}

// vectorFor is unique per article, so a vector on the wrong document shows.
func vectorFor(i int) []float32 {
	v := make([]float32, EmbeddingDims)
	for j := range v {
		v[j] = float32(i+1) / float32(j+1)
	}
	return v
}

func testVectors(articles []document.Article) []embedding.Vec {
	vecs := make([]embedding.Vec, len(articles))
	for i, a := range articles {
		vecs[i] = embedding.Vec{ID: a.ID, Model: loadTestModelTag, Embedding: vectorFor(i)}
	}
	return vecs
}

func corpusOf(articles []document.Article, vecs []embedding.Vec) storage.Corpus {
	c := storage.Corpus{
		Articles:  seqOf(articles),
		Language:  language.English,
		Canonical: storage.PackageRef{Name: "cc-news", Version: "article-v2-0000test", ResourceHash: "sha256:" + fmt.Sprintf("%064d", 0)},
	}
	if vecs != nil {
		c.Vectors = seqOf(vecs)
		c.ModelTag = loadTestModelTag
		c.Embeddings = &storage.PackageRef{Name: "cc-news-embeddings", Version: "lead-v1-1024-0000test", ResourceHash: "sha256:" + fmt.Sprintf("%064d", 1)}
	}
	return c
}

// refreshingMidway leaves a second segment, as a full corpus has, so force-merge has work.
func refreshingMidway(t *testing.T, address, index string, articles []document.Article) iter.Seq2[document.Article, error] {
	return func(yield func(document.Article, error) bool) {
		for i, a := range articles {
			if i == len(articles)/2 {
				var refreshed map[string]any
				postJSON(t, address+"/"+index+"/_refresh", "", &refreshed)
			}
			if !yield(a, nil) {
				return
			}
		}
	}
}

func seqOf[T any](items []T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for _, item := range items {
			if !yield(item, nil) {
				return
			}
		}
	}
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	decodeResponse(t, url, res, out)
}

func postJSON(t *testing.T, url, body string, out any) {
	t.Helper()
	sendJSON(t, http.MethodPost, url, body, out)
}

func sendJSON(t *testing.T, method, url, body string, out any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	decodeResponse(t, url, res, out)
}

func decodeResponse(t *testing.T, url string, res *http.Response, out any) {
	t.Helper()
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if res.StatusCode >= http.StatusMultipleChoices {
		t.Fatalf("%s: status %d: %s", url, res.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode %s: %v: %s", url, err, body)
	}
}

func assertJSONEqual(t *testing.T, name string, got json.RawMessage, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Errorf("%s: mapping %s is not JSON: %v", name, got, err)
		return
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("%s: want %s is not JSON: %v", name, want, err)
	}
	gotBytes, _ := json.Marshal(gotValue)
	wantBytes, _ := json.Marshal(wantValue)
	if string(gotBytes) != string(wantBytes) {
		t.Errorf("%s = %s, want %s", name, gotBytes, wantBytes)
	}
}
