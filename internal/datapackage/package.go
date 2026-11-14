// Package datapackage reads and writes Frictionless Data Package v2 descriptors.
package datapackage

import (
	"github.com/tadjov/tusker/internal/types/language"
	"github.com/tadjov/tusker/pkg/apis/datamapping"
)

const (
	Profile        = "https://datapackage.org/profiles/2.0/datapackage.json"
	DescriptorName = "datapackage.json"
	ArticleSchema  = "article/v2"
)

// Resource names fixed by the contract.
const (
	ArticlesResource     = "articles"
	EmbeddingsResource   = "embeddings"
	QueryVectorsResource = "query-vectors"
)

// Package is a data package descriptor.
type Package struct {
	Schema        string        `json:"$schema"`
	Name          string        `json:"name"`
	Version       string        `json:"version,omitempty"`
	ArticleSchema string        `json:"articleSchema,omitempty"`
	Language      language.Code `json:"language,omitempty"`
	IDStrategy    *IDStrategy   `json:"idStrategy,omitempty"`
	// Identity is the mapping's url identity rule; nil means ids follow the url as written.
	Identity   *datamapping.Identity `json:"identity,omitempty"`
	ProducedBy *ProducedBy           `json:"producedBy,omitempty"`
	Report     *Report               `json:"report,omitempty"`
	Embedding  *Embedding            `json:"embedding,omitempty"`
	Canonical  *Ref                  `json:"canonical,omitempty"`
	Raw        *RawRef               `json:"raw,omitempty"`
	Suite      *Suite                `json:"suite,omitempty"`
	Sources    []Source              `json:"sources,omitempty"`
	Resources  []Resource            `json:"resources" schema:"required,minItems=1"`
}

// Resource is one file of a package; Path is relative to the descriptor.
type Resource struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Format    string `json:"format"`
	MediaType string `json:"mediatype,omitempty"`
	Bytes     int64  `json:"bytes"`
	Hash      string `json:"hash"`
}

type Source struct {
	Title   string `json:"title,omitempty"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
}

// IDStrategy records how article ids were minted.
type IDStrategy struct {
	Kind      datamapping.IdStrategyKind `json:"kind"`
	Source    string                     `json:"source,omitempty"`
	Namespace string                     `json:"namespace,omitempty"`
}

type ProducedBy struct {
	Tool string `json:"tool"`
}

// Report counts rows through the producing stage; Dropped is keyed by reason.
type Report struct {
	Read    int64            `json:"read"`
	Written int64            `json:"written"`
	Dropped map[string]int64 `json:"dropped,omitempty"`
	Deduped int64            `json:"deduped"`
}

// Embedding describes how vectors were made; Limit is set only on a --limit trial.
type Embedding struct {
	Model            string `json:"model"`
	HuggingFaceModel string `json:"hfModel"`
	Revision         string `json:"revision"`
	Tag              string `json:"tag"`
	Dim              int    `json:"dim"`
	Recipe           string `json:"recipe"`
	Pooling          string `json:"pooling"`
	Normalized       bool   `json:"normalized"`
	MaxTokens        int    `json:"maxTokens"`
	BatchSize        int    `json:"batchSize"`
	DType            string `json:"dtype"`
	Limit            *int   `json:"limit,omitempty"`
}

// Ref names the canonical package an embeddings package was built from.
type Ref struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	ResourceHash string `json:"resourceHash"`
}

// RawRef names the raw package a canonical package was built from.
type RawRef struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Suite names the bench suite query vectors were embedded from.
type Suite struct {
	ID string `json:"id"`
}
