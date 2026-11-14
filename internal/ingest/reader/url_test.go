package reader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/pkg/apis/datamapping"
)

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "trims and lowercases the host", raw: "  https://Example.COM/Story  ", want: "https://example.com/Story"},
		{name: "keeps the fragment", raw: "https://example.com/story#comments", want: "https://example.com/story#comments"},
		{name: "keeps every parameter in its order", raw: "https://example.com/view?id=7&utm_campaign=x&page=2", want: "https://example.com/view?id=7&utm_campaign=x&page=2"},
		{name: "keeps the scheme and www", raw: "http://www.example.com/story/", want: "http://www.example.com/story/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NormalizeURL(tt.raw)
			require.True(t, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeURL_RejectsWhatIsNotAnAbsoluteHTTPURL(t *testing.T) {
	for _, raw := range []string{"", "   ", "/relative/path", "ftp://example.com/file", "mailto:a@example.com"} {
		t.Run(raw, func(t *testing.T) {
			_, ok := NormalizeURL(raw)
			assert.False(t, ok)
		})
	}
}

func TestURLIdentity_WithoutRulesIsTheURLAsNormalized(t *testing.T) {
	for _, raw := range []string{
		"https://example.com/a%20b",
		"http://www.example.com/story/?utm_source=feed#top",
		"https://example.com/view?b=2&a=1",
	} {
		normalized, ok := NormalizeURL(raw)
		require.True(t, ok)
		assert.Equal(t, normalized, URLIdentity(normalized, nil), raw)
	}
}

func TestURLIdentity_EachRuleMergesOnlyItsOwnVariant(t *testing.T) {
	rules := map[string]struct {
		ignore datamapping.IdentityIgnore
		a, b   string
	}{
		"scheme":        {datamapping.IdentityIgnore{Scheme: true}, "https://example.com/story", "http://example.com/story"},
		"www":           {datamapping.IdentityIgnore{WWW: true}, "https://example.com/story", "https://www.example.com/story"},
		"trailingSlash": {datamapping.IdentityIgnore{TrailingSlash: true}, "https://example.com/story", "https://example.com/story/"},
		"fragment":      {datamapping.IdentityIgnore{Fragment: true}, "https://example.com/story", "https://example.com/story#comments"},
		"queryParams":   {datamapping.IdentityIgnore{QueryParams: []string{"utm_*"}}, "https://example.com/story", "https://example.com/story?utm_source=feed"},
	}
	for name, rule := range rules {
		identity := &datamapping.Identity{Ignore: rule.ignore}
		for pairName, pair := range rules {
			t.Run(name+" on the "+pairName+" pair", func(t *testing.T) {
				same := identityOf(t, pair.a, identity) == identityOf(t, pair.b, identity)
				assert.Equal(t, name == pairName, same)
			})
		}
	}
}

func TestURLIdentity_IgnoresQueryParamsByNameOrPrefix(t *testing.T) {
	identity := &datamapping.Identity{Ignore: datamapping.IdentityIgnore{QueryParams: []string{"utm_*", "ref"}}}

	assert.Equal(t,
		identityOf(t, "https://example.com/view?id=7&page=2", identity),
		identityOf(t, "https://example.com/view?UTM_Source=x&id=7&ref=home&page=2&utm_medium=rss", identity))
	assert.NotEqual(t,
		identityOf(t, "https://example.com/view?id=7", identity),
		identityOf(t, "https://example.com/view?id=7&referrer=home", identity))
}

func TestURLIdentity_KeepsDifferentPagesApart(t *testing.T) {
	every := &datamapping.Identity{Ignore: datamapping.IdentityIgnore{
		Scheme: true, WWW: true, TrailingSlash: true, Fragment: true, QueryParams: []string{"utm_*"},
	}}
	pairs := [][2]string{
		{"https://example.com/view?id=7", "https://example.com/view?id=8"},
		{"https://example.com/a/story", "https://example.com/b/story"},
		{"https://news.example.com/story", "https://example.com/story"},
		{"https://example.com/Story", "https://example.com/story"},
	}
	for _, pair := range pairs {
		assert.NotEqual(t, identityOf(t, pair[0], every), identityOf(t, pair[1], every), "%s and %s", pair[0], pair[1])
	}
}

func identityOf(t *testing.T, raw string, identity *datamapping.Identity) string {
	t.Helper()
	normalized, ok := NormalizeURL(raw)
	require.True(t, ok, raw)
	return URLIdentity(normalized, identity)
}
