package storage

import (
	"context"
	"iter"
	"time"

	"github.com/DjordjeVuckovic/tusker/internal/embedding"
	"github.com/DjordjeVuckovic/tusker/internal/types/document"
	"github.com/DjordjeVuckovic/tusker/internal/types/language"
)

// CorpusLoader builds an engine's corpus from scratch.
type CorpusLoader interface {
	LoadCorpus(ctx context.Context, c Corpus, opts LoadOptions) (LoadRecord, error)
}

type Corpus struct {
	Articles iter.Seq2[document.Article, error]
	// Vectors is nil without embeddings, else one per article in Articles order, a prefix of them only for a --trial load.
	Vectors    iter.Seq2[embedding.Vec, error]
	Language   language.Code
	ModelTag   string
	Canonical  PackageRef
	Embeddings *PackageRef
}

type PackageRef struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	ResourceHash string `json:"resourceHash"`
}

type LoadOptions struct {
	BatchSize int
	SkipIndex bool
}

type StageTiming struct {
	Name    string  `json:"name"`
	Seconds float64 `json:"seconds"`
}

// LoadRecord is the provenance of one corpus load, Settings as the engine reports them.
type LoadRecord struct {
	Engine        string            `json:"engine"`
	EngineVersion string            `json:"engineVersion"`
	StartedAt     time.Time         `json:"startedAt"`
	FinishedAt    time.Time         `json:"finishedAt"`
	Canonical     PackageRef        `json:"canonical"`
	Embeddings    *PackageRef       `json:"embeddings"`
	ModelTag      string            `json:"modelTag"`
	Articles      int64             `json:"articles"`
	Vectors       int64             `json:"vectors"`
	BatchSize     int               `json:"batchSize"`
	Stages        []StageTiming     `json:"stages"`
	Settings      map[string]string `json:"settings"`
}
