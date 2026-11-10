package datapackage_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tadjov/tusker/internal/datapackage"
)

// The profile's path patterns use lookaheads, which Go's RE2 cannot compile.
type ecmaRegexp struct{ re *regexp2.Regexp }

func (r ecmaRegexp) MatchString(s string) bool {
	ok, err := r.re.MatchString(s)
	return err == nil && ok
}

func (r ecmaRegexp) String() string { return r.re.String() }

func compileSchema(t *testing.T, path, id string) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.UseRegexpEngine(func(expr string) (jsonschema.Regexp, error) {
		re, err := regexp2.Compile(expr, regexp2.ECMAScript)
		if err != nil {
			return nil, err
		}
		return ecmaRegexp{re}, nil
	})
	if err := c.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func compileProfile(t *testing.T) *jsonschema.Schema {
	t.Helper()
	return compileSchema(t, filepath.Join("testdata", "datapackage-profile-2.0.json"), datapackage.Profile)
}

func TestWrittenDescriptorConformsToDataPackageV2Profile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "articles.parquet", "not really parquet")
	writeFile(t, dir, "embeddings.parquet", "not really vectors")

	_, err := datapackage.Write(context.Background(), dir, fullPackage())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, datapackage.DescriptorName))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := compileProfile(t).Validate(doc); err != nil {
		t.Errorf("descriptor violates the v2 profile: %v\n%s", err, raw)
	}
}

func fullPackage() datapackage.Package {
	return datapackage.Package{
		Name:          "cc-news-canonical",
		Version:       "v1",
		ArticleSchema: datapackage.ArticleSchema,
		Language:      "eng",
		IDStrategy:    &datapackage.IDStrategy{Kind: "uuidv5", Source: "url", Namespace: "6ba7b811-9dad-11d1-80b4-00c04fd430c8"},
		ProducedBy:    &datapackage.ProducedBy{Tool: "datapipe preprocess"},
		Report: &datapackage.Report{
			Read: 10, Written: 7, Deduped: 1,
			Dropped: map[string]int64{"invalid_url": 2},
		},
		Embedding: &datapackage.Embedding{
			Model: "qwen3-embedding:0.6b", HuggingFaceModel: "Qwen/Qwen3-Embedding-0.6B", Revision: "abc123",
			Tag: "qwen3-embedding:0.6b@1024", Dim: 1024, Recipe: "lead-v1", Pooling: "last_token",
			Normalized: true, MaxTokens: 512, BatchSize: 64, DType: "float16",
		},
		Canonical: &datapackage.Ref{Name: "cc-news-canonical", Version: "v1", ResourceHash: "sha256:" + zeros64},
		Sources:   []datapackage.Source{{Title: "huggingface vblagoje/cc_news", Path: "https://huggingface.co/datasets/vblagoje/cc_news", Version: "81eb2ce0d2a9dad6ad16b68ef750ec290880fa36"}},
		Resources: []datapackage.Resource{
			{Name: datapackage.ArticlesResource, Path: "articles.parquet", Format: "parquet"},
			{Name: datapackage.EmbeddingsResource, Path: "embeddings.parquet", Format: "parquet"},
		},
	}
}

const zeros64 = "0000000000000000000000000000000000000000000000000000000000000000"

// Python producers don't use this package, so names are checked against the contract.
func TestWrittenDescriptorUsesContractPropertyNames(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "articles.parquet", "not really parquet")
	writeFile(t, dir, "embeddings.parquet", "not really vectors")
	if _, err := datapackage.Write(context.Background(), dir, fullPackage()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	doc := readDescriptorJSON(t, dir)

	want := map[string]any{
		"$schema":                    "https://datapackage.org/profiles/2.0/datapackage.json",
		"name":                       "cc-news-canonical",
		"version":                    "v1",
		"articleSchema":              "article/v2",
		"language":                   "eng",
		"idStrategy/kind":            "uuidv5",
		"producedBy/tool":            "datapipe preprocess",
		"report/read":                10.0,
		"report/written":             7.0,
		"report/dropped/invalid_url": 2.0,
		"report/deduped":             1.0,
		"embedding/model":            "qwen3-embedding:0.6b",
		"embedding/hfModel":          "Qwen/Qwen3-Embedding-0.6B",
		"embedding/revision":         "abc123",
		"embedding/tag":              "qwen3-embedding:0.6b@1024",
		"embedding/dim":              1024.0,
		"embedding/recipe":           "lead-v1",
		"embedding/pooling":          "last_token",
		"embedding/normalized":       true,
		"embedding/maxTokens":        512.0,
		"embedding/batchSize":        64.0,
		"embedding/dtype":            "float16",
		"canonical/name":             "cc-news-canonical",
		"canonical/version":          "v1",
		"canonical/resourceHash":     "sha256:" + zeros64,
	}
	for pointer, value := range want {
		if got := lookup(doc, pointer); got != value {
			t.Errorf("%s = %v, want %v", pointer, got, value)
		}
	}
	if _, ok := doc["embedding"].(map[string]any)["limit"]; ok {
		t.Errorf("embedding.limit is written for a package that is not a trial: %v", doc["embedding"])
	}
	resources := doc["resources"].([]any)
	for i, name := range []string{"articles", "embeddings"} {
		resource := resources[i].(map[string]any)
		if resource["name"] != name {
			t.Errorf("resource %d is named %v, want %s", i, resource["name"], name)
		}
		for _, key := range []string{"name", "path", "format", "bytes", "hash"} {
			if _, ok := resource[key]; !ok {
				t.Errorf("resource lacks %q: %v", key, resource)
			}
		}
	}
}

func lookup(doc map[string]any, pointer string) any {
	var node any = doc
	for _, key := range strings.Split(pointer, "/") {
		object, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = object[key]
	}
	return node
}
