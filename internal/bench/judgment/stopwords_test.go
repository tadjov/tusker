package judgment

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
)

// luceneEnglishStopwords is Lucene's _english_ list, which Elasticsearch and
// ParadeDB stop.
var luceneEnglishStopwords = []string{
	"a", "an", "and", "are", "as", "at", "be", "but", "by", "for", "if", "in", "into", "is", "it", "no", "not",
	"of", "on", "or", "such", "that", "the", "their", "then", "there", "these", "they", "this", "to", "was",
	"will", "with",
}

func TestStopwords_ArePostgresEnglishStopAndHoldLucenes(t *testing.T) {
	sum := sha256.Sum256([]byte(englishStop))
	assert.Equal(t, "b3f772a000465cb76e23adb03b47073c591c156fad8f7af09c8b8e80d6bd8eac", hex.EncodeToString(sum[:]),
		"english.stop is no longer Postgres 18's file byte for byte")
	assert.Len(t, stopwords, 127)
	for _, w := range luceneEnglishStopwords {
		assert.Contains(t, stopwords, w)
	}
}
