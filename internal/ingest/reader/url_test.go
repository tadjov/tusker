package reader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "trims and lowercases the host", raw: "  https://Example.COM/Story  ", want: "https://example.com/Story"},
		{name: "drops the fragment", raw: "https://example.com/story#comments", want: "https://example.com/story"},
		{name: "drops utm parameters", raw: "https://example.com/story?utm_source=feed&utm_medium=rss", want: "https://example.com/story"},
		{name: "keeps other parameters in their order", raw: "https://example.com/view?id=7&utm_campaign=x&page=2", want: "https://example.com/view?id=7&page=2"},
		{name: "keeps the scheme", raw: "http://example.com/story", want: "http://example.com/story"},
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

func TestURLIdentity_NamesOnePageAcrossItsSpellings(t *testing.T) {
	spellings := []string{
		"https://bleacherreport.com/articles/2784436-nba-trade-rumors",
		"http://bleacherreport.com/articles/2784436-nba-trade-rumors",
		"https://www.bleacherreport.com/articles/2784436-nba-trade-rumors",
		"https://bleacherreport.com/articles/2784436-nba-trade-rumors/",
		"https://bleacherreport.com/articles/2784436-nba-trade-rumors#comments",
		"http://bleacherreport.com/articles/2784436-nba-trade-rumors?utm_source=feedburner&utm_medium=feed",
	}
	want := identityOf(t, spellings[0])
	for _, raw := range spellings[1:] {
		assert.Equal(t, want, identityOf(t, raw), raw)
	}
}

func TestURLIdentity_KeepsDifferentPagesApart(t *testing.T) {
	pairs := [][2]string{
		{"https://example.com/view?id=7", "https://example.com/view?id=8"},
		{"https://example.com/a/story", "https://example.com/b/story"},
		{"https://news.example.com/story", "https://example.com/story"},
		{"https://example.com/Story", "https://example.com/story"},
	}
	for _, pair := range pairs {
		assert.NotEqual(t, identityOf(t, pair[0]), identityOf(t, pair[1]), "%s and %s", pair[0], pair[1])
	}
}

func identityOf(t *testing.T, raw string) string {
	t.Helper()
	normalized, ok := NormalizeURL(raw)
	require.True(t, ok, raw)
	return URLIdentity(normalized)
}
