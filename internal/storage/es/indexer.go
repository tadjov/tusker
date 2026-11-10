package es

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esutil"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
)

type Indexer struct {
	client       *elasticsearch.TypedClient
	indexName    string
	config       ClientConfig
	indexBuilder *IndexBuilder
}

func NewIndexer(ctx context.Context, config ClientConfig) (*Indexer, error) {
	client, err := newTypedClient(config)

	if err != nil {
		return nil, fmt.Errorf("failed to create Elasticsearch client: %w", err)
	}
	storer := &Indexer{
		client:       client,
		indexName:    config.IndexName,
		config:       config,
		indexBuilder: NewIndexBuilder(),
	}

	if err := storer.EnsureIndex(ctx); err != nil {
		return nil, fmt.Errorf("failed to ensure index exists: %w", err)
	}

	return storer, nil
}

func (e *Indexer) Save(ctx context.Context, article document.Article) (uuid.UUID, error) {
	doc, err := e.indexBuilder.mapToESDocument(article)
	if err != nil {
		return uuid.Nil, err
	}

	res, err := e.client.Index(e.indexName).Id(doc.ID).Document(doc).Do(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to index document: %w", err)
	}

	articleID, err := uuid.Parse(doc.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to parse article ID: %w", err)
	}

	slog.Info("document indexed successfully", "id", doc.ID, "index", e.indexName, "result", res.Result)
	return articleID, nil
}

func (e *Indexer) SaveBulk(ctx context.Context, articles []document.Article) error {
	if len(articles) == 0 {
		return nil
	}

	docs := make([]ArticleDocument, len(articles))
	for i, article := range articles {
		doc, err := e.indexBuilder.mapToESDocument(article)
		if err != nil {
			return err
		}
		docs[i] = doc
	}

	bi, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
		Index:         e.indexName,
		Client:        e.client,
		NumWorkers:    4,
		FlushBytes:    5e+6, // 5MB
		FlushInterval: 30 * time.Second,
	})

	if err != nil {
		return fmt.Errorf("failed to create bulk indexer: %w", err)
	}

	for _, doc := range docs {
		docBytes, err := json.Marshal(doc)
		if err != nil {
			slog.Error("failed to marshal document", "error", err, "id", doc.ID)
			continue
		}

		err = bi.Add(
			ctx,
			esutil.BulkIndexerItem{
				Action:     "index",
				DocumentID: doc.ID,
				Body:       bytes.NewReader(docBytes),
				OnFailure: func(ctx context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem, err error) {
					if err != nil {
						slog.Error("bulk index error", "error", err, "id", item.DocumentID)
					} else {
						slog.Error("bulk index error", "status", res.Status, "error", res.Error.Type, "reason", res.Error.Reason, "id", item.DocumentID)
					}
				},
			},
		)
		if err != nil {
			slog.Error("failed to add document to bulk indexer", "error", err, "id", doc.ID)
		}
	}

	// Close the indexer and wait for completion
	if err := bi.Close(ctx); err != nil {
		return fmt.Errorf("failed to close bulk indexer: %w", err)
	}

	stats := bi.Stats()
	indexed := stats.NumCreated + stats.NumUpdated

	slog.Info("Bulk indexing completed",
		"indexed", indexed,
		"created", stats.NumCreated,
		"updated", stats.NumUpdated,
		"failed", stats.NumFailed,
		"total", len(articles),
		"index", e.indexName)

	if stats.NumFailed > 0 {
		return fmt.Errorf("failed to index %d out of %d articles", stats.NumFailed, len(articles))
	}

	return nil
}

func (e *Indexer) EnsureIndex(ctx context.Context) error {
	existsRes, err := e.client.Indices.Exists(e.indexName).Do(ctx)
	if err != nil {
		return fmt.Errorf("failed to check if index exists: %w", err)
	}

	if existsRes {
		slog.Info("Index already exists", "index", e.indexName)
		return nil
	}

	settings := e.indexBuilder.buildSettings()

	mappings := e.indexBuilder.buildMapping(language.English.Analysis().ElasticsearchAnalyzer)

	createRes, err := e.client.Indices.Create(e.indexName).
		Settings(&settings).
		Mappings(&mappings).
		Do(ctx)
	if err != nil {
		return fmt.Errorf("failed to create index: %w", err)
	}

	if !createRes.Acknowledged {
		return fmt.Errorf("index creation was not acknowledged")
	}

	slog.Info("Index created successfully", "index", e.indexName)
	return nil
}

// CountEmbedded reports how many documents already carry a vector.
func (e *Indexer) CountEmbedded(ctx context.Context) (int64, error) {
	res, err := e.client.Count().
		Index(e.indexName).
		Query(&types.Query{Exists: &types.ExistsQuery{Field: "embedding"}}).
		Do(ctx)
	if err != nil {
		return 0, fmt.Errorf("count embedded documents: %w", err)
	}
	return res.Count, nil
}

var _ storage.EmbeddedDocumentCounter = (*Indexer)(nil)
