package datapackage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/tadjov/tusker/internal/datapackage"
)

var descriptorSchemaPath = filepath.Join("..", "..", "api", "datapackage-spec", "datapackage-v1.json")

func compileDescriptorSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	return compileSchema(t, descriptorSchemaPath, "datapackage-v1.json")
}

func validateDescriptor(t *testing.T, schema *jsonschema.Schema, descriptor []byte) error {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(descriptor))
	if err != nil {
		t.Fatal(err)
	}
	return schema.Validate(doc)
}

func embeddingsPackage() datapackage.Package {
	return datapackage.Package{
		Name:       "cc-news-embeddings",
		Version:    "v1",
		ProducedBy: &datapackage.ProducedBy{Tool: "scripts/embed_corpus.py"},
		Embedding: &datapackage.Embedding{
			Model: "qwen3-embedding:0.6b", HuggingFaceModel: "Qwen/Qwen3-Embedding-0.6B", Revision: "abc123",
			Tag: "qwen3-embedding:0.6b@1024", Dim: 1024, Recipe: "lead-v1", Pooling: "last_token",
			Normalized: true, MaxTokens: 512, BatchSize: 32, DType: "float16",
		},
		Canonical: &datapackage.Ref{Name: "cc-news-canonical", Version: "v1", ResourceHash: "sha256:" + zeros64},
		Resources: []datapackage.Resource{{Name: datapackage.EmbeddingsResource, Path: "embeddings.parquet", Format: "parquet"}},
	}
}

func representativePackages() map[string]datapackage.Package {
	limit := 500
	trial := embeddingsPackage()
	trial.Version = "v1-trial"
	trialEmbedding := *trial.Embedding
	trialEmbedding.Limit = &limit
	trial.Embedding = &trialEmbedding

	return map[string]datapackage.Package{
		"canonical": {
			Name:          "cc-news-canonical",
			Version:       "v1",
			ArticleSchema: datapackage.ArticleSchema,
			Language:      "eng",
			IDStrategy:    &datapackage.IDStrategy{Kind: "uuidv5", Source: "url", Namespace: "6ba7b811-9dad-11d1-80b4-00c04fd430c8"},
			ProducedBy:    &datapackage.ProducedBy{Tool: "datapipe preprocess"},
			Report:        &datapackage.Report{Read: 10, Written: 7, Deduped: 1, Dropped: map[string]int64{"invalid_url": 2}},
			Sources:       []datapackage.Source{{Title: "Hugging Face vblagoje/cc_news", Path: "https://huggingface.co/datasets/vblagoje/cc_news", Version: "81eb2ce"}},
			Resources:     []datapackage.Resource{{Name: datapackage.ArticlesResource, Path: "articles.parquet", Format: "parquet"}},
		},
		"embeddings":       embeddingsPackage(),
		"embeddings trial": trial,
		"query vectors":    queryVectorsPackage(),
		"raw": {
			Name:       "cc-news-raw",
			Version:    "81eb2ce0d2a9dad6ad16b68ef750ec290880fa36",
			ProducedBy: &datapackage.ProducedBy{Tool: "scripts/fetch_hf_dataset.py"},
			Sources:    []datapackage.Source{{Title: "Hugging Face vblagoje/cc_news", Path: "https://huggingface.co/datasets/vblagoje/cc_news", Version: "81eb2ce0d2a9dad6ad16b68ef750ec290880fa36"}},
			Resources: []datapackage.Resource{
				{Name: "train-00000-of-00002.parquet", Path: "train-00000-of-00002.parquet", Format: "parquet"},
				{Name: "train-00001-of-00002.parquet", Path: "train-00001-of-00002.parquet", Format: "parquet"},
			},
		},
		"every property": fullPackage(),
	}
}

func writeDescriptor(t *testing.T, pkg datapackage.Package) []byte {
	t.Helper()
	dir := t.TempDir()
	for _, r := range pkg.Resources {
		writeFile(t, dir, r.Path, "placeholder for "+r.Name)
	}
	if _, err := datapackage.Write(context.Background(), dir, pkg); err != nil {
		t.Fatalf("Write: %v", err)
	}
	descriptor, err := os.ReadFile(filepath.Join(dir, datapackage.DescriptorName))
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func TestEveryWrittenDescriptorShapeConformsToTheDescriptorSchema(t *testing.T) {
	schema := compileDescriptorSchema(t)
	for name, pkg := range representativePackages() {
		t.Run(name, func(t *testing.T) {
			if err := validateDescriptor(t, schema, writeDescriptor(t, pkg)); err != nil {
				t.Errorf("descriptor violates %s: %v", descriptorSchemaPath, err)
			}
		})
	}
}

func edit(t *testing.T, descriptor []byte, change func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(descriptor, &doc); err != nil {
		t.Fatal(err)
	}
	change(doc)
	edited, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return edited
}

// rejectedAsUnknown reports whether err refuses property as unlisted, not for another reason.
func rejectedAsUnknown(err error, property string) bool {
	var validation *jsonschema.ValidationError
	if !errors.As(err, &validation) {
		return false
	}
	var found func(*jsonschema.ValidationError) bool
	found = func(e *jsonschema.ValidationError) bool {
		if unknown, ok := e.ErrorKind.(*kind.AdditionalProperties); ok && slices.Contains(unknown.Properties, property) {
			return true
		}
		return slices.ContainsFunc(e.Causes, found)
	}
	return found(validation)
}

func TestDescriptorSchemaRejectsKeysTheContractDoesNotList(t *testing.T) {
	schema := compileDescriptorSchema(t)
	valid := writeDescriptor(t, embeddingsPackage())
	if err := validateDescriptor(t, schema, valid); err != nil {
		t.Fatalf("the unedited descriptor must validate: %v", err)
	}
	embedding := func(doc map[string]any) map[string]any { return doc["embedding"].(map[string]any) }

	tests := []struct {
		name     string
		property string
		change   func(doc map[string]any)
	}{
		{"misspelled embedding key", "batchsize", func(doc map[string]any) {
			embedding(doc)["batchsize"] = embedding(doc)["batchSize"]
			delete(embedding(doc), "batchSize")
		}},
		{"misspelled key next to the right one", "batchsize", func(doc map[string]any) {
			embedding(doc)["batchsize"] = 32
		}},
		{"unknown top-level key", "createdAt", func(doc map[string]any) {
			doc["createdAt"] = "2026-10-17T00:00:00Z"
		}},
		{"retired mapping hash", "mapping", func(doc map[string]any) {
			doc["mapping"] = map[string]any{"path": "mapping.yaml", "hash": "sha256:" + zeros64}
		}},
		{"retired producedBy commit", "commit", func(doc map[string]any) {
			doc["producedBy"] = map[string]any{"tool": "scripts/embed_corpus.py", "commit": "c4ecbe3"}
		}},
		{"retired suite hash", "hash", func(doc map[string]any) {
			doc["suite"] = map[string]any{"id": "cc-news-fts", "hash": "sha256:" + zeros64}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDescriptor(t, schema, edit(t, valid, tt.change))
			if !rejectedAsUnknown(err, tt.property) {
				t.Errorf("err = %v, want %s refused as an unlisted property", err, tt.property)
			}
		})
	}
}

func TestAResourceMediaTypeIsKeptAndAllowed(t *testing.T) {
	const parquet = "application/vnd.apache.parquet"
	pkg := embeddingsPackage()
	pkg.Resources[0].MediaType = parquet
	dir := t.TempDir()
	writeFile(t, dir, "embeddings.parquet", "placeholder")

	written, err := datapackage.Write(context.Background(), dir, pkg)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if written.Resources[0].MediaType != parquet {
		t.Errorf("Write returned mediatype %q, want %q", written.Resources[0].MediaType, parquet)
	}
	descriptor, err := os.ReadFile(filepath.Join(dir, datapackage.DescriptorName))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDescriptor(t, compileDescriptorSchema(t), descriptor); err != nil {
		t.Errorf("a resource with mediatype violates %s: %v", descriptorSchemaPath, err)
	}
	opened, err := datapackage.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := opened.Package.Resources[0].MediaType; got != parquet {
		t.Errorf("Open returned mediatype %q, want %q", got, parquet)
	}
}
