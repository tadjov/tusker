package datamapping

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/tadjov/tusker/internal/types/language"
)

// DataMapper defines field mapping configuration for data imports
// +schema:root=true
// +schema:group=tusker.sh
// +schema:version=v1
type DataMapper struct {
	// Kind is the resource type identifier
	Kind string `json:"kind" yaml:"kind" schema:"required,enum=DataMapper" description:"Resource type identifier"`

	// Version is the API version
	Version string `json:"version" yaml:"version" schema:"required,enum=v1" description:"API version"`

	// Metadata contains the mapping metadata
	Metadata Metadata `json:"metadata" yaml:"metadata" schema:"required" description:"Mapping metadata"`

	// Dataset is the source dataset identifier
	Dataset string `json:"dataset" yaml:"dataset" schema:"required,pattern=^[a-z0-9-_]+$,minLength=1,maxLength=50" description:"Dataset source identifier"`

	// DateFormat specifies the Go time format for parsing dates
	DateFormat string `json:"dateFormat,omitempty" yaml:"dateFormat,omitempty" schema:"default=2006-01-02T15:04:05Z" description:"Go time format for parsing date/datetime fields"`

	// IdStrategy decides how article ids are produced
	IdStrategy IdStrategy `json:"idStrategy,omitempty" yaml:"idStrategy,omitempty" description:"How article ids are produced"`

	// Identity decides which url differences still name the same article
	Identity *Identity `json:"identity,omitempty" yaml:"identity,omitempty" description:"Which url differences still name the same article; left out, the id follows the url as written"`

	// Constants are written on every article
	Constants Constants `json:"constants" yaml:"constants" schema:"required" description:"Values written on every article"`

	// Output configures the canonical package
	Output Output `json:"output" yaml:"output" schema:"required" description:"Canonical package output"`

	// FieldMappings defines the field mapping rules
	FieldMappings []FieldMapping `json:"fieldMappings" yaml:"fieldMappings" schema:"required,minItems=1" description:"Array of field mapping definitions"`
}

// IdStrategyKind names the ways an article id can be produced.
type IdStrategyKind string

// IdStrategyUuidV5 derives the id from the normalised url.
const IdStrategyUuidV5 IdStrategyKind = "uuidv5"

func ParseIdStrategyKind(v string) (IdStrategyKind, error) {
	if IdStrategyKind(v) == IdStrategyUuidV5 {
		return IdStrategyUuidV5, nil
	}
	return "", fmt.Errorf("unknown id strategy kind %q, want %q: ids derive from the normalised url", v, IdStrategyUuidV5)
}

type IdStrategy struct {
	// Kind is the id generation strategy
	Kind string `json:"kind,omitempty" yaml:"kind,omitempty" schema:"enum=uuidv5,default=uuidv5" description:"Id generation strategy"`

	// Source must be the column mapped to URL
	Source string `json:"source,omitempty" yaml:"source,omitempty" description:"Source column mapped to URL, hashed by uuidv5; defaults to that column"`
}

// ResolvedKind defaults to uuidv5.
func (s IdStrategy) ResolvedKind() (IdStrategyKind, error) {
	if s.Kind == "" {
		return IdStrategyUuidV5, nil
	}
	return ParseIdStrategyKind(s.Kind)
}

func (s IdStrategy) validate(urlSource string) error {
	kind, err := s.ResolvedKind()
	if err != nil {
		return err
	}
	if s.Source != "" && s.Source != urlSource {
		return fmt.Errorf("id strategy %q hashes the url, so its source must be %q, the column mapped to URL, not %q", kind, urlSource, s.Source)
	}
	return nil
}

// Identity decides which urls name one article: the id hashes the url with these parts ignored.
type Identity struct {
	Ignore IdentityIgnore `json:"ignore" yaml:"ignore" schema:"required" description:"Url parts that do not change which article a url names"`
}

type IdentityIgnore struct {
	Scheme        bool     `json:"scheme,omitempty" yaml:"scheme,omitempty" description:"http and https name one page"`
	WWW           bool     `json:"www,omitempty" yaml:"www,omitempty" description:"www.example.com and example.com name one page"`
	TrailingSlash bool     `json:"trailingSlash,omitempty" yaml:"trailingSlash,omitempty" description:"/story and /story/ name one page"`
	Fragment      bool     `json:"fragment,omitempty" yaml:"fragment,omitempty" description:"/story#comments and /story name one page"`
	QueryParams   []string `json:"queryParams,omitempty" yaml:"queryParams,omitempty" description:"Query parameters that do not change the page, by name; a trailing * matches a prefix, as in utm_*"`
}

// IgnoresQueryParam matches key case-insensitively.
func (i IdentityIgnore) IgnoresQueryParam(key string) bool {
	key = strings.ToLower(key)
	for _, pattern := range i.QueryParams {
		pattern = strings.ToLower(pattern)
		if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		} else if key == pattern {
			return true
		}
	}
	return false
}

func (i Identity) validate() error {
	ignore := i.Ignore
	if !ignore.Scheme && !ignore.WWW && !ignore.TrailingSlash && !ignore.Fragment && len(ignore.QueryParams) == 0 {
		return errors.New("ignore names no url part; leave identity out to keep the url as written")
	}
	for _, pattern := range ignore.QueryParams {
		if name := strings.TrimSuffix(pattern, "*"); name == "" || strings.Contains(name, "*") {
			return fmt.Errorf("query param %q: want a name, or a prefix followed by one trailing *", pattern)
		}
	}
	return nil
}

type Constants struct {
	// Language is the ISO 639-3 corpus language
	Language string `json:"Language" yaml:"Language" schema:"required,enum=eng" description:"ISO 639-3 corpus language written on every article"`
}

func (c Constants) ResolvedLanguage() (language.Code, error) {
	if c.Language == "" {
		return "", errors.New("constants.Language is required")
	}
	lang, err := language.Parse(c.Language)
	if err != nil {
		return "", fmt.Errorf("constants.Language: %w", err)
	}
	return lang, nil
}

type OutputFormat string

const (
	OutputParquet OutputFormat = "parquet"
	OutputJSONL   OutputFormat = "jsonl"
)

func ParseOutputFormat(v string) (OutputFormat, error) {
	switch OutputFormat(v) {
	case OutputParquet:
		return OutputParquet, nil
	case OutputJSONL:
		return OutputJSONL, nil
	default:
		return "", fmt.Errorf("unknown output format %q, want %q or %q", v, OutputParquet, OutputJSONL)
	}
}

type Output struct {
	// Format is the articles file format
	Format string `json:"format" yaml:"format" schema:"required,enum=parquet|jsonl" description:"Canonical articles file format"`
}

func (o Output) ResolvedFormat() (OutputFormat, error) {
	if o.Format == "" {
		return "", errors.New("output.format is required")
	}
	format, err := ParseOutputFormat(o.Format)
	if err != nil {
		return "", fmt.Errorf("output.format: %w", err)
	}
	return format, nil
}

type Metadata struct {
	// Name is the human-readable name for the mapping
	Name string `json:"name" yaml:"name" schema:"required,minLength=1,maxLength=100" description:"Human-readable name for the mapping configuration"`

	// Description provides details about the mapping
	Description string `json:"description,omitempty" yaml:"description,omitempty" schema:"maxLength=500" description:"Description of the mapping configuration"`
}

// SourceType is how a raw value is parsed.
type SourceType string

const (
	SourceString   SourceType = "string"
	SourceInt      SourceType = "int"
	SourceFloat    SourceType = "float"
	SourceBool     SourceType = "bool"
	SourceDate     SourceType = "date"
	SourceDatetime SourceType = "datetime"
	SourceURL      SourceType = "url"
)

func ParseSourceType(v string) (SourceType, error) {
	switch t := SourceType(v); t {
	case SourceString, SourceInt, SourceFloat, SourceBool, SourceDate, SourceDatetime, SourceURL:
		return t, nil
	default:
		return "", fmt.Errorf("unknown source type %q", v)
	}
}

type FieldMapping struct {
	// Source is the field name in the source dataset
	Source string `json:"source" yaml:"source" schema:"required,minLength=1,maxLength=100" description:"Source field name in the dataset"`

	// SourceType is the data type of the source field
	SourceType string `json:"sourceType,omitempty" yaml:"sourceType,omitempty" schema:"enum=string|int|float|bool|date|datetime|url,default=string" description:"Source field data type; url normalises the value, and an invalid url counts as missing"`

	// Target is the article field the value lands in
	Target string `json:"target" yaml:"target" schema:"required,pattern=^(URL|Title|Description|Content|PublishedAt|Metadata\\.(ExternalID|PublisherName|Authors|Section|Keywords|Image|DateModified)|Metadata\\.Extra\\.[A-Za-z][A-Za-z0-9_]*)$" description:"Article field, or Metadata.Extra.<key> for a dataset-specific value"`

	// Separator splits the source value of a list target
	Separator string `json:"separator,omitempty" yaml:"separator,omitempty" description:"Splits the value of a list target (Metadata.Authors, Metadata.Keywords)"`

	// Required indicates if this field mapping is mandatory
	Required bool `json:"required,omitempty" yaml:"required,omitempty" schema:"default=false" description:"Whether a record missing this value is dropped; Title and URL must set it"`
}

// ResolvedSourceType defaults to string.
func (fm FieldMapping) ResolvedSourceType() (SourceType, error) {
	if fm.SourceType == "" {
		return SourceString, nil
	}
	return ParseSourceType(fm.SourceType)
}

func IsListTarget(target string) bool {
	return targets[target].list
}

func (dm *DataMapper) Validate() error {
	if dm.Kind != "DataMapper" {
		return fmt.Errorf("kind %q, want %q", dm.Kind, "DataMapper")
	}
	if dm.Version != "v1" {
		return fmt.Errorf("version %q, want %q", dm.Version, "v1")
	}
	if n := utf8.RuneCountInString(dm.Metadata.Name); n == 0 || n > 100 {
		return fmt.Errorf("metadata.name must be 1 to 100 characters, got %d", n)
	}
	if n := utf8.RuneCountInString(dm.Metadata.Description); n > 500 {
		return fmt.Errorf("metadata.description must be at most 500 characters, got %d", n)
	}
	if len(dm.Dataset) > 50 || !datasetPattern.MatchString(dm.Dataset) {
		return fmt.Errorf("dataset %q must match %s and be at most 50 characters", dm.Dataset, datasetPattern)
	}
	if _, err := dm.Constants.ResolvedLanguage(); err != nil {
		return err
	}
	if _, err := dm.Output.ResolvedFormat(); err != nil {
		return err
	}
	if len(dm.FieldMappings) == 0 {
		return errors.New("at least one field mapping is required")
	}
	byTarget := make(map[string]FieldMapping, len(dm.FieldMappings))
	for i, fm := range dm.FieldMappings {
		if err := fm.validate(); err != nil {
			return fmt.Errorf("fieldMappings[%d]: %w", i, err)
		}
		if previous, mapped := byTarget[fm.Target]; mapped {
			return fmt.Errorf("fieldMappings[%d]: target %q is already mapped from source %q, and two sources cannot share a target", i, fm.Target, previous.Source)
		}
		byTarget[fm.Target] = fm
	}
	for _, target := range requiredTargets {
		fm, mapped := byTarget[target]
		if !mapped {
			return fmt.Errorf("target %q must be mapped, since a record without it is dropped", target)
		}
		if !fm.Required {
			return fmt.Errorf("target %q must set required: true, since a record without it is dropped", target)
		}
	}
	if err := dm.IdStrategy.validate(byTarget[TargetURL].Source); err != nil {
		return fmt.Errorf("idStrategy: %w", err)
	}
	if dm.Identity != nil {
		if err := dm.Identity.validate(); err != nil {
			return fmt.Errorf("identity: %w", err)
		}
	}
	return nil
}

func (fm FieldMapping) validate() error {
	if n := utf8.RuneCountInString(fm.Source); n == 0 || n > 100 {
		return fmt.Errorf("source must be 1 to 100 characters, got %d", n)
	}
	sourceType, err := fm.ResolvedSourceType()
	if err != nil {
		return err
	}
	spec, err := lookupTarget(fm.Target)
	if err != nil {
		return err
	}
	if spec.sourceTypes != nil && !slices.Contains(spec.sourceTypes, sourceType) {
		return fmt.Errorf("target %q cannot hold a %q source, want one of %q", fm.Target, sourceType, spec.sourceTypes)
	}
	if fm.Separator != "" && !spec.list {
		return fmt.Errorf("target %q holds one value, so it takes no separator", fm.Target)
	}
	return nil
}

const (
	TargetURL = "URL"
	// TargetTitle is required so every article has lead-v1 embedding text.
	TargetTitle = "Title"
)

// ExtraTargetPrefix marks a target that lands in Metadata.Extra rather than a
// schema field. Anything after the prefix is the key.
const ExtraTargetPrefix = "Metadata.Extra."

var requiredTargets = []string{TargetURL, TargetTitle}

var (
	datasetPattern  = regexp.MustCompile(`^[a-z0-9-_]+$`)
	extraKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
)

// targetSpec with nil sourceTypes accepts any source type.
type targetSpec struct {
	sourceTypes []SourceType
	list        bool
}

var (
	textSource = []SourceType{SourceString}
	timeSource = []SourceType{SourceDate, SourceDatetime}
)

var targets = map[string]targetSpec{
	TargetURL:                {sourceTypes: []SourceType{SourceURL}},
	TargetTitle:              {sourceTypes: textSource},
	"Description":            {sourceTypes: textSource},
	"Content":                {sourceTypes: textSource},
	"PublishedAt":            {sourceTypes: timeSource},
	"Metadata.ExternalID":    {sourceTypes: textSource},
	"Metadata.PublisherName": {sourceTypes: textSource},
	"Metadata.Authors":       {sourceTypes: textSource, list: true},
	"Metadata.Section":       {sourceTypes: textSource},
	"Metadata.Keywords":      {sourceTypes: textSource, list: true},
	"Metadata.Image":         {sourceTypes: []SourceType{SourceString, SourceURL}},
	"Metadata.DateModified":  {sourceTypes: timeSource},
}

func lookupTarget(target string) (targetSpec, error) {
	if target == "" {
		return targetSpec{}, errors.New("must have target defined")
	}
	if spec, ok := targets[target]; ok {
		return spec, nil
	}
	if key, ok := strings.CutPrefix(target, ExtraTargetPrefix); ok {
		if !extraKeyPattern.MatchString(key) {
			return targetSpec{}, fmt.Errorf("target %q needs a key matching %s after %q", target, extraKeyPattern, ExtraTargetPrefix)
		}
		return targetSpec{}, nil
	}
	return targetSpec{}, fmt.Errorf("unknown target %q", target)
}
