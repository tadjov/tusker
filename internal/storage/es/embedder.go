package es

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esutil"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage"
)

// EmbeddingDims is the vector width produced by the embedding model and declared
// on the article index mapping. Must match the loaded embeddings file.
const EmbeddingDims = 1024

// Embedder is the Elasticsearch implementation of storage.EmbedIndexer. Unlike
// Postgres (a separate article_embeddings table), it stores the vector as a
// dense_vector field on the existing article document, keyed by _id == article
// UUID. Writes are partial updates, so they are idempotent and re-runnable;
// updates targeting a non-existent article (orphan) are skipped, not fatal.
type Embedder struct {
	client    *elasticsearch.TypedClient
	indexName string
}

func NewEmbedder(ctx context.Context, config ClientConfig) (*Embedder, error) {
	client, err := newTypedClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Elasticsearch client: %w", err)
	}

	e := &Embedder{client: client, indexName: config.IndexName}
	if err := e.ensureEmbeddingField(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

// embeddingUpdate is the partial-update body: {"doc": {"embedding": [...], ...}}.
type embeddingUpdate struct {
	Doc embeddingDoc `json:"doc"`
}

type embeddingDoc struct {
	Embedding []float32 `json:"embedding"`
	Model     string    `json:"embedding_model"`
}

func (e *Embedder) Save(ctx context.Context, vec *embedding.Vec) (uuid.UUID, error) {
	res, err := e.SaveBulk(ctx, []*embedding.Vec{vec})
	if err != nil {
		return uuid.Nil, err
	}
	if res.Stored == 0 {
		return uuid.Nil, fmt.Errorf("embedding not stored: no article %s", vec.ID)
	}
	return vec.ID, nil
}

func (e *Embedder) SaveBulk(ctx context.Context, vecs []*embedding.Vec) (storage.EmbedWriteResult, error) {
	if len(vecs) == 0 {
		return storage.EmbedWriteResult{}, nil
	}

	bi, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
		Index:         e.indexName,
		Client:        e.client,
		NumWorkers:    4,
		FlushBytes:    5e+6,
		FlushInterval: 30 * time.Second,
	})
	if err != nil {
		return storage.EmbedWriteResult{}, fmt.Errorf("failed to create bulk indexer: %w", err)
	}

	var orphans, otherFailures int64
	for _, vec := range vecs {
		body, err := json.Marshal(embeddingUpdate{Doc: embeddingDoc{
			Embedding: vec.Embedding,
			Model:     vec.Model,
		}})
		if err != nil {
			slog.Error("failed to marshal embedding update", "error", err, "id", vec.ID)
			continue
		}

		err = bi.Add(ctx, esutil.BulkIndexerItem{
			Action:     "update",
			DocumentID: vec.ID.String(),
			Body:       bytes.NewReader(body),
			OnFailure: func(_ context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem, err error) {
				// 404 == no such article (orphan): expected, skip like Postgres.
				if err == nil && res.Status == 404 {
					atomic.AddInt64(&orphans, 1)
					return
				}
				atomic.AddInt64(&otherFailures, 1)
				if err != nil {
					slog.Error("bulk embedding update error", "error", err, "id", item.DocumentID)
				} else {
					slog.Error("bulk embedding update error", "status", res.Status, "type", res.Error.Type, "reason", res.Error.Reason, "id", item.DocumentID)
				}
			},
		})
		if err != nil {
			slog.Error("failed to add embedding to bulk indexer", "error", err, "id", vec.ID)
		}
	}

	if err := bi.Close(ctx); err != nil {
		return storage.EmbedWriteResult{}, fmt.Errorf("failed to close bulk indexer: %w", err)
	}

	stats := bi.Stats()
	result := storage.EmbedWriteResult{
		Stored:  int(stats.NumUpdated),
		Skipped: int(orphans),
	}
	if orphans > 0 {
		slog.Warn("skipped embeddings with no matching article", "skipped", orphans, "updated", stats.NumUpdated)
	}
	if otherFailures > 0 {
		return result, fmt.Errorf("failed to update %d embeddings", otherFailures)
	}
	// A flush that fails at the transport level, or on a non-2xx, counts its
	// whole batch in NumFailed without ever calling the per-item OnFailure. Left
	// unchecked those rows would be reported as skipped, with a nil error.
	if unattributed := int64(stats.NumFailed) - orphans - otherFailures; unattributed > 0 {
		return result, fmt.Errorf("elasticsearch rejected %d embeddings in a failed bulk request", unattributed)
	}
	return result, nil
}

// ensureEmbeddingField adds the dense_vector field to an existing index when it
// is missing (PUT mapping is a no-op when the field already matches). New
// indices already get it via Indexer.EnsureIndex.
func (e *Embedder) ensureEmbeddingField(ctx context.Context) error {
	if err := e.checkVectorBuildParams(ctx); err != nil {
		return err
	}

	builder := NewIndexBuilder()
	props := map[string]types.Property{
		"embedding":       builder.denseVectorProperty(),
		"embedding_model": types.NewKeywordProperty(),
	}
	if _, err := e.client.Indices.PutMapping(e.indexName).Properties(props).Do(ctx); err != nil {
		return fmt.Errorf("failed to add embedding field to index %q: %w", e.indexName, err)
	}
	return nil
}

// checkVectorBuildParams stops a load into a graph built at other HNSW
// parameters. Elasticsearch will not change index_options on an existing field,
// and the graph is assembled as documents are written, so such an index has to
// be dropped and reloaded.
func (e *Embedder) checkVectorBuildParams(ctx context.Context) error {
	res, err := e.client.Indices.GetMapping().Index(e.indexName).Do(ctx)
	if err != nil {
		return fmt.Errorf("failed to read mapping of index %q: %w", e.indexName, err)
	}
	record, ok := res[e.indexName]
	if !ok {
		return nil
	}
	vector, ok := record.Mappings.Properties["embedding"].(*types.DenseVectorProperty)
	if !ok {
		return nil
	}
	if buildParamsMatchDeclared(vector.IndexOptions) {
		return nil
	}
	return &VectorIndexMismatchError{Index: e.indexName, Options: vector.IndexOptions}
}

func buildParamsMatchDeclared(opts *types.DenseVectorIndexOptions) bool {
	if opts == nil || opts.M == nil || opts.EfConstruction == nil {
		return false
	}
	return *opts.M == storage.HNSWM && *opts.EfConstruction == storage.HNSWEfConstruction
}

// VectorIndexMismatchError reports an index whose vector field was built at
// different HNSW parameters than the ones this build declares.
type VectorIndexMismatchError struct {
	Index   string
	Options *types.DenseVectorIndexOptions
}

func (e *VectorIndexMismatchError) Error() string {
	return fmt.Sprintf(
		"index %q holds a vector field built with %s, but this build declares m=%d, ef_construction=%d: "+
			"elasticsearch cannot change index_options on an existing field, and the HNSW graph is assembled "+
			"as documents are written — delete the index and reload it",
		e.Index, describeBuildParams(e.Options), storage.HNSWM, storage.HNSWEfConstruction)
}

func describeBuildParams(opts *types.DenseVectorIndexOptions) string {
	if opts == nil || opts.M == nil || opts.EfConstruction == nil {
		return "undeclared index_options, so at the elasticsearch defaults"
	}
	return fmt.Sprintf("m=%d, ef_construction=%d", *opts.M, *opts.EfConstruction)
}
