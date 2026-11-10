package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/api/dto"
	apiserver "github.com/tadjov/tusker/internal/api/server"
	"github.com/tadjov/tusker/internal/apperr"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
	dquery "github.com/tadjov/tusker/internal/types/query"
)

type oneArticleSearcher struct {
	stubFtsSearcher
	article document.Article
}

func (s oneArticleSearcher) SearchStringQuery(context.Context, *dquery.String, *dquery.BaseOptions) (*storage.SearchResult, error) {
	return &storage.SearchResult{
		Hits: []dto.ArticleSearchResult{{Article: dto.NewArticle(s.article), Score: 1}},
	}, nil
}

func TestSearchHandler_ReturnsTheArticleShape(t *testing.T) {
	article := document.Article{
		ID:              uuid.MustParse("6f1c0d53-1b0e-5a51-9a2b-7d0f6c3e8a01"),
		URL:             "https://uk.reuters.com/world/storm",
		Title:           "Storm hits the coast",
		Description:     "Thousands without power",
		Content:         "The storm made landfall overnight.",
		PublishedAt:     time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC),
		Language:        language.English,
		PublisherDomain: "reuters.com",
		Metadata: document.Metadata{
			ExternalID:    "reuters-0412",
			PublisherName: "Reuters",
			Authors:       []string{"Jane Doe"},
			Section:       "World",
			Keywords:      []string{"storm"},
			Image:         "https://uk.reuters.com/storm.jpg",
			DateModified:  time.Date(2019, 3, 5, 0, 0, 0, 0, time.UTC),
			Extra:         map[string]any{"wordCount": "412"},
		},
	}
	bare := document.Article{
		ID:       uuid.MustParse("6f1c0d53-1b0e-5a51-9a2b-7d0f6c3e8a02"),
		URL:      "https://example.com/bare",
		Title:    "Bare",
		Content:  "",
		Language: language.English,
	}

	tests := []struct {
		name    string
		article document.Article
		want    string
	}{
		{
			name:    "every field",
			article: article,
			want: `{
				"id": "6f1c0d53-1b0e-5a51-9a2b-7d0f6c3e8a01",
				"url": "https://uk.reuters.com/world/storm",
				"title": "Storm hits the coast",
				"description": "Thousands without power",
				"content": "The storm made landfall overnight.",
				"publishedAt": "2019-03-04T05:06:07Z",
				"language": "eng",
				"publisherDomain": "reuters.com",
				"metadata": {
					"externalId": "reuters-0412",
					"publisherName": "Reuters",
					"authors": ["Jane Doe"],
					"section": "World",
					"keywords": ["storm"],
					"image": "https://uk.reuters.com/storm.jpg",
					"dateModified": "2019-03-05T00:00:00Z",
					"extra": {"wordCount": "412"}
				}
			}`,
		},
		{
			name:    "no optional field",
			article: bare,
			want: `{
				"id": "6f1c0d53-1b0e-5a51-9a2b-7d0f6c3e8a02",
				"url": "https://example.com/bare",
				"title": "Bare",
				"content": "",
				"language": "eng"
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			(&apiserver.Server{Echo: e}).SetupValidator()
			e.HTTPErrorHandler = apperr.GlobalErrorHandler()
			(&SearchRouter{e: e, searcher: oneArticleSearcher{article: tt.article}}).Bind()

			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/articles/search?q=storm", nil))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			var body struct {
				Hits []struct {
					Article json.RawMessage `json:"article"`
				} `json:"hits"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Len(t, body.Hits, 1)
			assert.JSONEq(t, tt.want, string(body.Hits[0].Article))
		})
	}
}
