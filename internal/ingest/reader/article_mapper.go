package reader

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/internal/types/language"
	"github.com/tadjov/tusker/pkg/apis/datamapping"
	"github.com/tadjov/tusker/pkg/utils"
)

type ArticleMapper struct {
	cfg      *datamapping.DataMapper
	language language.Code
}

func NewArticleMapper(cfg *datamapping.DataMapper) (*ArticleMapper, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	lang, err := cfg.Constants.ResolvedLanguage()
	if err != nil {
		return nil, err
	}
	return &ArticleMapper{cfg: cfg, language: lang}, nil
}

// Map returns a *MappingDropError when a required value is missing or invalid.
func (m *ArticleMapper) Map(record map[string]string) (document.Article, error) {
	article := document.Article{Language: m.language}
	val := reflect.ValueOf(&article).Elem()

	for _, fm := range m.cfg.FieldMappings {
		raw, present := record[fm.Source]
		if !present {
			if fm.Required {
				return document.Article{}, &MappingDropError{Reason: ReasonMissingColumn, Source: fm.Source, Target: fm.Target}
			}
			continue
		}

		if strings.TrimSpace(raw) == "" {
			if fm.Required {
				return document.Article{}, &MappingDropError{Reason: ReasonEmptyValue, Source: fm.Source, Target: fm.Target}
			}
			continue
		}

		converted, err := m.convert(raw, fm)
		if err == nil && isBlank(converted) {
			err = fmt.Errorf("value %q converted to an empty %s", raw, fm.SourceType)
		}
		if err == nil {
			err = m.assign(val, &article, fm.Target, converted)
		}
		if err != nil {
			if fm.Required {
				reason := ReasonConversion
				if errors.Is(err, errInvalidURL) {
					reason = ReasonInvalidURL
				}
				return document.Article{}, &MappingDropError{Reason: reason, Source: fm.Source, Target: fm.Target, Err: err}
			}
			slog.Warn("skipping optional field", "field", fm.Target, "error", err)
		}
	}

	article.ID = document.DeriveArticleID(URLIdentity(article.URL))
	article.PublisherDomain = PublisherDomain(article.URL)
	return article, nil
}

var errInvalidURL = errors.New("not an absolute http(s) url")

func (m *ArticleMapper) convert(raw string, fm datamapping.FieldMapping) (any, error) {
	sourceType, err := fm.ResolvedSourceType()
	if err != nil {
		return nil, err
	}
	if datamapping.IsListTarget(fm.Target) {
		return splitList(raw, fm.Separator), nil
	}
	if sourceType == datamapping.SourceURL {
		normalized, ok := NormalizeURL(raw)
		if !ok {
			return nil, fmt.Errorf("value %q: %w", raw, errInvalidURL)
		}
		return normalized, nil
	}
	return convertValueToType(raw, string(sourceType), m.cfg.DateFormat)
}

// splitList without a separator keeps raw as one element.
func splitList(raw, separator string) []string {
	parts := []string{raw}
	if separator != "" {
		parts = strings.Split(raw, separator)
	}
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			values = append(values, part)
		}
	}
	return values
}

// assign routes a converted value to its target. Metadata.Extra.<key> is a
// struct hop then a map key, which reflection cannot walk in one pass, so it is
// matched by prefix ahead of the ordinary struct paths.
func (m *ArticleMapper) assign(val reflect.Value, article *document.Article, target string, converted any) error {
	if key, ok := strings.CutPrefix(target, datamapping.ExtraTargetPrefix); ok {
		if article.Metadata.Extra == nil {
			article.Metadata.Extra = make(map[string]any)
		}
		article.Metadata.Extra[key] = converted
		return nil
	}

	if path := strings.Split(target, "."); len(path) > 1 {
		return AssignNestedField(val, path, converted)
	}
	return AssignField(val, target, converted)
}

func isBlank(converted any) bool {
	switch v := converted.(type) {
	case string:
		return v == ""
	case time.Time:
		return v.IsZero()
	case []string:
		return len(v) == 0
	default:
		return false
	}
}

// ArticleDirectMapper reads records already in canonical form.
type ArticleDirectMapper struct{}

func NewArticleDirectMapper() *ArticleDirectMapper {
	return &ArticleDirectMapper{}
}

func (m *ArticleDirectMapper) Map(record map[string]string) (document.Article, error) {
	id, err := uuid.Parse(record["id"])
	if err != nil {
		return document.Article{}, fmt.Errorf("invalid id: %w", err)
	}

	lang, err := language.Parse(record["language"])
	if err != nil {
		return document.Article{}, fmt.Errorf("article %s: %w", id, err)
	}

	publishedAt, err := utils.ParseTimeOptional(record["publishedAt"])
	if err != nil {
		return document.Article{}, fmt.Errorf("article %s: invalid publishedAt: %w", id, err)
	}

	var metadata document.Metadata
	if raw := record["metadata"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
			return document.Article{}, fmt.Errorf("article %s: invalid metadata: %w", id, err)
		}
	}

	return document.Article{
		ID:              id,
		URL:             record["url"],
		Title:           record["title"],
		Description:     record["description"],
		Content:         record["content"],
		PublishedAt:     publishedAt,
		Language:        lang,
		PublisherDomain: record["publisherDomain"],
		Metadata:        metadata,
	}, nil
}
