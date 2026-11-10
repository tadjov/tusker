package es

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/api/dto"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
	dquery "github.com/tadjov/tusker/internal/types/query"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

func TestIndexer(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewESContainer(ctx, t)
	cfg := ClientConfig{Addresses: []string{container.Address}, IndexName: "articles_indexer_test"}

	indexer, err := NewIndexer(ctx, cfg)
	require.NoError(t, err)

	t.Run("stores and returns every article field", func(t *testing.T) {
		savedArticleRoundTrips(t, cfg, indexer)
	})
	t.Run("refuses an article without a corpus language", func(t *testing.T) {
		for _, lang := range []language.Code{"", "english", "en"} {
			_, err := indexer.Save(ctx, document.Article{ID: uuid.New(), Title: "t", Language: lang})
			assert.Error(t, err, "language %q", lang)
		}
	})
}

func savedArticleRoundTrips(t *testing.T, cfg ClientConfig, indexer *Indexer) {
	ctx := context.Background()

	published := time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC)
	modified := time.Date(2019, 3, 5, 0, 0, 0, 0, time.UTC)
	full := document.Article{
		ID:              uuid.MustParse("6f1c0d53-1b0e-5a51-9a2b-7d0f6c3e8a01"),
		URL:             "https://uk.reuters.com/world/storm",
		Title:           "Storm hits the coast",
		Description:     "Thousands without power",
		Content:         "The storm made landfall overnight.",
		PublishedAt:     published,
		Language:        language.English,
		PublisherDomain: "reuters.com",
		Metadata: document.Metadata{
			ExternalID:    "reuters-0412",
			PublisherName: "Reuters",
			Authors:       []string{"Jane Doe", "John Roe"},
			Section:       "World",
			Keywords:      []string{"storm", "coast"},
			Image:         "https://uk.reuters.com/storm.jpg",
			DateModified:  modified,
			Extra:         map[string]any{"wordCount": "412"},
		},
	}
	require.NoError(t, indexer.SaveBulk(ctx, []document.Article{full}))

	client, err := newTypedClient(cfg)
	require.NoError(t, err)
	got, err := client.Get(cfg.IndexName, full.ID.String()).Do(ctx)
	require.NoError(t, err)
	require.True(t, got.Found, "article %s not stored", full.ID)

	assert.JSONEq(t, `{
		"id": "6f1c0d53-1b0e-5a51-9a2b-7d0f6c3e8a01",
		"url": "https://uk.reuters.com/world/storm",
		"title": "Storm hits the coast",
		"description": "Thousands without power",
		"content": "The storm made landfall overnight.",
		"published_at": "2019-03-04T05:06:07Z",
		"language": "eng",
		"publisher_domain": "reuters.com",
		"metadata": {
			"externalId": "reuters-0412",
			"publisherName": "Reuters",
			"authors": ["Jane Doe", "John Roe"],
			"section": "World",
			"keywords": ["storm", "coast"],
			"image": "https://uk.reuters.com/storm.jpg",
			"dateModified": "2019-03-05T00:00:00Z",
			"extra": {"wordCount": "412"}
		}
	}`, string(got.Source_))

	_, err = client.Indices.Refresh().Index(cfg.IndexName).Do(ctx)
	require.NoError(t, err)
	res, err := NewSearcher(newTestClient(t, cfg)).SearchStringQuery(ctx, &dquery.String{Query: "storm"}, &dquery.BaseOptions{Size: 10})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, dto.Article{
		ID:              full.ID,
		URL:             full.URL,
		Title:           full.Title,
		Description:     full.Description,
		Content:         full.Content,
		PublishedAt:     &published,
		Language:        "eng",
		PublisherDomain: "reuters.com",
		Metadata: dto.Metadata{
			ExternalID:    "reuters-0412",
			PublisherName: "Reuters",
			Authors:       []string{"Jane Doe", "John Roe"},
			Section:       "World",
			Keywords:      []string{"storm", "coast"},
			Image:         "https://uk.reuters.com/storm.jpg",
			DateModified:  &modified,
			Extra:         map[string]any{"wordCount": "412"},
		},
	}, res.Hits[0].Article)
}
