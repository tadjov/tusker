package reader_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/ingest/reader"
	"github.com/tadjov/tusker/internal/types/language"
	"github.com/tadjov/tusker/pkg/apis/datamapping"
	"gopkg.in/yaml.v3"
)

const validMapping = `kind: DataMapper
version: v1
metadata:
  name: Test
dataset: test
constants:
  Language: eng
output:
  format: jsonl
fieldMappings:
  - source: title
    target: Title
    required: true
  - source: link
    sourceType: url
    target: URL
    required: true
`

const repoRoot = "../../.."

func loadMapping(doc string) (*datamapping.DataMapper, error) {
	return reader.NewYAMLConfigLoader(strings.NewReader(doc)).Load(true)
}

func withMappings(doc string, fieldMappings ...string) string {
	return doc + strings.Join(fieldMappings, "")
}

func replaced(doc, old, replacement string) string {
	if !strings.Contains(doc, old) {
		panic("fixture does not contain " + old)
	}
	return strings.Replace(doc, old, replacement, 1)
}

func TestLoadMapping_AcceptsEveryTargetKind(t *testing.T) {
	doc := withMappings(replaced(validMapping, "format: jsonl", "format: parquet"),
		"  - {source: body, target: Content}\n",
		"  - {source: summary, target: Description}\n",
		"  - {source: date, sourceType: datetime, target: PublishedAt}\n",
		"  - {source: outlet, target: Metadata.PublisherName}\n",
		"  - {source: byline, target: Metadata.Authors, separator: ';'}\n",
		"  - {source: tags, target: Metadata.Keywords, separator: ','}\n",
		"  - {source: section, target: Metadata.Section}\n",
		"  - {source: image_url, sourceType: url, target: Metadata.Image}\n",
		"  - {source: modified, sourceType: date, target: Metadata.DateModified}\n",
		"  - {source: words, sourceType: int, target: Metadata.Extra.wordCount}\n",
	)

	cfg, err := loadMapping(doc)
	require.NoError(t, err)

	lang, err := cfg.Constants.ResolvedLanguage()
	require.NoError(t, err)
	assert.Equal(t, language.English, lang)
	format, err := cfg.Output.ResolvedFormat()
	require.NoError(t, err)
	assert.Equal(t, datamapping.OutputParquet, format)
}

var unknownKeyMappings = []struct {
	name    string
	doc     string
	wantKey string
}{
	{
		name:    "top level",
		doc:     validMapping + "idStrategey:\n  kind: uuidv5\n  source: link\n",
		wantKey: "idStrategey",
	},
	{
		name:    "inside idStrategy",
		doc:     validMapping + "idStrategy:\n  kidn: uuidv5\n",
		wantKey: "kidn",
	},
	{
		name:    "inside a field mapping",
		doc:     replaced(validMapping, "    target: Title\n", "    target: Title\n    requried: true\n"),
		wantKey: "requried",
	},
	{
		name:    "the retired targetType",
		doc:     replaced(validMapping, "    target: Title\n", "    target: Title\n    targetType: string\n"),
		wantKey: "targetType",
	},
}

func TestLoadMapping_RejectsAnUnknownKeyByName(t *testing.T) {
	for _, tt := range unknownKeyMappings {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadMapping(tt.doc)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantKey)
		})
	}
}

// crossField marks rules the JSON schema cannot express, so only Go rejects them.
var rejectedMappings = []struct {
	name       string
	doc        string
	wantInErr  string
	crossField bool
}{
	{name: "a retired target", doc: withMappings(validMapping, "  - {source: author, target: Author}\n"), wantInErr: `"Author"`},
	{name: "a retired metadata target", doc: withMappings(validMapping, "  - {source: domain, target: Metadata.SourceName}\n"), wantInErr: "Metadata.SourceName"},
	{name: "a mapped publisher domain", doc: withMappings(validMapping, "  - {source: domain, target: PublisherDomain}\n"), wantInErr: "PublisherDomain"},
	{name: "language as a mapped target", doc: withMappings(validMapping, "  - {source: lang, target: Language}\n"), wantInErr: `"Language"`},
	{name: "an extra key with no name", doc: withMappings(validMapping, "  - {source: x, target: Metadata.Extra.}\n"), wantInErr: "Metadata.Extra."},
	{name: "an extra key with a dot", doc: withMappings(validMapping, "  - {source: x, target: Metadata.Extra.a.b}\n"), wantInErr: "Metadata.Extra.a.b"},
	{name: "an unknown source type", doc: withMappings(validMapping, "  - {source: body, sourceType: text, target: Content}\n"), wantInErr: `"text"`},
	{name: "an unsupported language", doc: replaced(validMapping, "Language: eng", "Language: en"), wantInErr: `"en"`},
	{name: "no constants", doc: replaced(validMapping, "constants:\n  Language: eng\n", ""), wantInErr: "Language"},
	{name: "an unknown output format", doc: replaced(validMapping, "format: jsonl", "format: csv"), wantInErr: `"csv"`},
	{name: "no output", doc: replaced(validMapping, "output:\n  format: jsonl\n", ""), wantInErr: "format"},
	{name: "the wrong kind", doc: replaced(validMapping, "kind: DataMapper", "kind: DataMapping"), wantInErr: "DataMapping"},
	{name: "an unknown id strategy", doc: validMapping + "idStrategy:\n  kind: uuid5\n  source: link\n", wantInErr: "uuid5"},
	{name: "a random id strategy", doc: validMapping + "idStrategy:\n  kind: random\n", wantInErr: `"random"`},
	{
		name:       "two sources on one target",
		doc:        withMappings(validMapping, "  - {source: full_content, target: Content}\n", "  - {source: content, target: Content}\n"),
		wantInErr:  `"Content"`,
		crossField: true,
	},
	{
		name:       "two sources on one extra key",
		doc:        withMappings(validMapping, "  - {source: a, target: Metadata.Extra.k}\n", "  - {source: b, target: Metadata.Extra.k}\n"),
		wantInErr:  "Metadata.Extra.k",
		crossField: true,
	},
	{name: "no url", doc: replaced(validMapping, "  - source: link\n    sourceType: url\n    target: URL\n    required: true\n", ""), wantInErr: "URL", crossField: true},
	{name: "an optional title", doc: replaced(validMapping, "    target: Title\n    required: true\n", "    target: Title\n"), wantInErr: "Title", crossField: true},
	{name: "a url read as a plain string", doc: replaced(validMapping, "sourceType: url\n    target: URL", "sourceType: string\n    target: URL"), wantInErr: "URL", crossField: true},
	{name: "a date read into a text target", doc: withMappings(validMapping, "  - {source: body, sourceType: datetime, target: Content}\n"), wantInErr: "datetime", crossField: true},
	{name: "a separator on a single-valued target", doc: withMappings(validMapping, "  - {source: body, target: Content, separator: ';'}\n"), wantInErr: "separator", crossField: true},
	{name: "a uuidv5 id over a column other than the url", doc: validMapping + "idStrategy:\n  kind: uuidv5\n  source: title\n", wantInErr: `"title"`, crossField: true},
	{name: "a mapped article id", doc: withMappings(validMapping, "  - {source: uid, target: ID}\n"), wantInErr: `"ID"`},
	{name: "an external id read as a number", doc: withMappings(validMapping, "  - {source: uid, sourceType: int, target: Metadata.ExternalID}\n"), wantInErr: "int", crossField: true},
	{name: "the retired targetType", doc: replaced(validMapping, "    target: Title\n", "    target: Title\n    targetType: string\n"), wantInErr: "targetType"},
	{name: "a 101-character Cyrillic name", doc: replaced(validMapping, "name: Test", "name: "+strings.Repeat("ж", 101)), wantInErr: "metadata.name"},
	{name: "a 501-character Cyrillic description", doc: replaced(validMapping, "name: Test", "name: Test\n  description: "+strings.Repeat("ж", 501)), wantInErr: "metadata.description"},
	{name: "a 101-character Cyrillic source", doc: replaced(validMapping, "source: title", "source: "+strings.Repeat("ж", 101)), wantInErr: "source"},
}

func TestLoadMapping_RejectsAnInvalidMappingPrecisely(t *testing.T) {
	for _, tt := range rejectedMappings {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadMapping(tt.doc)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantInErr)
		})
	}
}

func compileMappingSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "api/mapping-spec/datamapping-v1.json"))
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(t, err)
	compiler := jsonschema.NewCompiler()
	require.NoError(t, compiler.AddResource("datamapping-v1.json", doc))
	schema, err := compiler.Compile("datamapping-v1.json")
	require.NoError(t, err)
	return schema
}

func validateAgainstSchema(t *testing.T, schema *jsonschema.Schema, doc string) error {
	t.Helper()
	var decoded any
	require.NoError(t, yaml.Unmarshal([]byte(doc), &decoded))
	encoded, err := json.Marshal(decoded)
	require.NoError(t, err)
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	require.NoError(t, err)
	return schema.Validate(instance)
}

func TestMappingSchema_AgreesWithTheLoader(t *testing.T) {
	schema := compileMappingSchema(t)

	accepted := []struct {
		name string
		doc  string
	}{
		{name: "a valid mapping", doc: validMapping},
		{name: "a 100-character Cyrillic name", doc: replaced(validMapping, "name: Test", "name: "+strings.Repeat("ж", 100))},
		{name: "a 500-character Cyrillic description", doc: replaced(validMapping, "name: Test", "name: Test\n  description: "+strings.Repeat("ж", 500))},
		{name: "a 100-character Cyrillic source", doc: replaced(validMapping, "source: title", "source: "+strings.Repeat("ж", 100))},
	}
	for _, tt := range accepted {
		t.Run("accepts "+tt.name, func(t *testing.T) {
			_, err := loadMapping(tt.doc)
			require.NoError(t, err)
			require.NoError(t, validateAgainstSchema(t, schema, tt.doc))
		})
	}
	for _, tt := range rejectedMappings {
		if tt.crossField {
			continue
		}
		t.Run("rejects "+tt.name, func(t *testing.T) {
			assert.Error(t, validateAgainstSchema(t, schema, tt.doc))
		})
	}
	for _, tt := range unknownKeyMappings {
		t.Run("rejects an unknown key "+tt.name, func(t *testing.T) {
			assert.Error(t, validateAgainstSchema(t, schema, tt.doc))
		})
	}
}

func TestCommittedMappings_LoadAndMatchTheSchema(t *testing.T) {
	schema := compileMappingSchema(t)
	paths, err := filepath.Glob(filepath.Join(repoRoot, "datasets/*/mapping.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	paths = append(paths, filepath.Join(repoRoot, "api/mapping-spec/example/datamapping-example.yaml"))

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			_, err = loadMapping(string(raw))
			require.NoError(t, err)
			require.NoError(t, validateAgainstSchema(t, schema, string(raw)))
		})
	}
}
