package datapackage_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/tadjov/tusker/internal/datapackage"
)

func queryVectorsPackage() datapackage.Package {
	return datapackage.Package{
		Name: "cc-news-fts-query-vectors",
		Embedding: &datapackage.Embedding{
			Model: "qwen3-embedding:0.6b", HuggingFaceModel: "Qwen/Qwen3-Embedding-0.6B", Revision: "abc123",
			Tag: "qwen3-embedding:0.6b@1024", Dim: 1024, Recipe: "query-instruct-v1", Pooling: "last_token",
			Normalized: true, MaxTokens: 512, BatchSize: 1, DType: "float16",
		},
		Suite: &datapackage.Suite{ID: "cc-news-fts"},
		Resources: []datapackage.Resource{
			{Name: datapackage.QueryVectorsResource, Path: "query-vectors.parquet", Format: "parquet"},
		},
	}
}

func TestQueryVectorsSuiteRoundTripsThroughWriteAndOpen(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "query-vectors.parquet", "not really vectors")
	written, err := datapackage.Write(context.Background(), dir, queryVectorsPackage())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	doc := readDescriptorJSON(t, dir)
	if lookup(doc, "suite/id") != "cc-news-fts" {
		t.Errorf("suite = %v, want {id: cc-news-fts}", doc["suite"])
	}
	opened, err := datapackage.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !reflect.DeepEqual(opened.Package, written) {
		t.Errorf("descriptor differs:\n got %#v\nwant %#v", opened.Package, written)
	}
	if opened.Package.Suite == nil || *opened.Package.Suite != (datapackage.Suite{ID: "cc-news-fts"}) {
		t.Errorf("opened suite = %+v", opened.Package.Suite)
	}
}

func TestSuiteIsRefusedOutsideAQueryVectorsPackage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "articles.parquet", "not really parquet")
	writeFile(t, dir, "embeddings.parquet", "not really vectors")
	pkg := fullPackage()
	pkg.Suite = &datapackage.Suite{ID: "cc-news-fts"}

	if _, err := datapackage.Write(context.Background(), dir, pkg); err == nil {
		t.Error("Write accepted a suite on a package without a query-vectors resource")
	}
}

func TestATrialLimitRoundTripsAndIsAbsentOtherwise(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "query-vectors.parquet", "not really vectors")
	trial := queryVectorsPackage()
	limit := 2000
	trial.Embedding.Limit = &limit
	written, err := datapackage.Write(context.Background(), dir, trial)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := lookup(readDescriptorJSON(t, dir), "embedding/limit"); got != 2000.0 {
		t.Errorf("embedding.limit = %v, want 2000", got)
	}
	opened, err := datapackage.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !reflect.DeepEqual(opened.Package, written) || opened.Package.Embedding.Limit == nil || *opened.Package.Embedding.Limit != 2000 {
		t.Errorf("opened embedding = %+v, want limit 2000", opened.Package.Embedding)
	}
}

// Python writes these descriptors, so this one is spelled out by hand.
func TestOpenDecodesAnEmbeddingsDescriptorFromThePythonProducer(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "embeddings.parquet", "hello\n")
	writeFile(t, dir, datapackage.DescriptorName, `{
  "$schema": "https://datapackage.org/profiles/2.0/datapackage.json",
  "name": "cc-news-embeddings",
  "version": "v1-trial",
  "embedding": {
    "model": "qwen3-embedding:0.6b", "hfModel": "Qwen/Qwen3-Embedding-0.6B", "revision": "abc123",
    "tag": "qwen3-embedding:0.6b@1024", "dim": 1024, "recipe": "lead-v1", "pooling": "last_token",
    "normalized": true, "maxTokens": 512, "batchSize": 32, "dtype": "float16", "limit": 500
  },
  "canonical": {"name": "cc-news-canonical", "version": "v1", "resourceHash": "sha256:`+zeros64+`"},
  "resources": [{"name": "embeddings", "path": "embeddings.parquet", "format": "parquet", "bytes": 6,
    "hash": "sha256:5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"}]
}`)

	opened, err := datapackage.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	limit := 500
	want := datapackage.Embedding{
		Model: "qwen3-embedding:0.6b", HuggingFaceModel: "Qwen/Qwen3-Embedding-0.6B", Revision: "abc123",
		Tag: "qwen3-embedding:0.6b@1024", Dim: 1024, Recipe: "lead-v1", Pooling: "last_token",
		Normalized: true, MaxTokens: 512, BatchSize: 32, DType: "float16", Limit: &limit,
	}
	if opened.Package.Embedding == nil || !reflect.DeepEqual(*opened.Package.Embedding, want) {
		t.Errorf("embedding = %+v, want %+v", opened.Package.Embedding, want)
	}
	if got := opened.Package.Canonical; got == nil || got.ResourceHash != "sha256:"+zeros64 {
		t.Errorf("canonical = %+v", got)
	}
}
