package document

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/types/language"
)

func TestArticle_JSONRoundTripKeepsEveryField(t *testing.T) {
	article := Article{
		ID:              uuid.MustParse("0c6f3f4e-6a53-5a8e-9a43-0f3b8d1c2e11"),
		URL:             "https://uk.reuters.com/world/storm",
		Title:           "Storm hits the coast",
		Description:     "Thousands without power",
		Content:         "The storm made landfall overnight.",
		PublishedAt:     time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC),
		Language:        language.English,
		PublisherDomain: "reuters.com",
		Metadata: Metadata{
			ExternalID:    "reuters-0412",
			PublisherName: "Reuters",
			Authors:       []string{"Jane Doe", "John Roe"},
			Section:       "World",
			Keywords:      []string{"storm", "coast"},
			Image:         "https://uk.reuters.com/storm.jpg",
			DateModified:  time.Date(2019, 3, 5, 0, 0, 0, 0, time.UTC),
			Extra:         map[string]any{"wordCount": "412"},
		},
	}
	const wantJSON = `{
		"id": "0c6f3f4e-6a53-5a8e-9a43-0f3b8d1c2e11",
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
			"authors": ["Jane Doe", "John Roe"],
			"section": "World",
			"keywords": ["storm", "coast"],
			"image": "https://uk.reuters.com/storm.jpg",
			"dateModified": "2019-03-05T00:00:00Z",
			"extra": {"wordCount": "412"}
		}
	}`

	encoded, err := json.Marshal(article)
	require.NoError(t, err)
	assert.JSONEq(t, wantJSON, string(encoded))

	var decoded Article
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, article, decoded)
}

func TestArticle_JSONOmitsUnsetOptionalFields(t *testing.T) {
	article := Article{
		ID:       uuid.MustParse("0c6f3f4e-6a53-5a8e-9a43-0f3b8d1c2e11"),
		URL:      "https://example.com/a",
		Title:    "Title",
		Content:  "",
		Language: language.English,
	}

	encoded, err := json.Marshal(article)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"id": "0c6f3f4e-6a53-5a8e-9a43-0f3b8d1c2e11",
		"url": "https://example.com/a",
		"title": "Title",
		"content": "",
		"language": "eng"
	}`, string(encoded))
}
