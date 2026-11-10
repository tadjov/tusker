package ingest

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/tadjov/tusker/internal/types/document"
)

type CanonicalWriter interface {
	Write(ar []document.Article) error
	Close() error
}

type JsonlCanonicalWriter struct {
	enc *json.Encoder
}

func NewJsonlCanonicalWriter(writer io.Writer) *JsonlCanonicalWriter {
	return &JsonlCanonicalWriter{
		enc: json.NewEncoder(writer),
	}
}

func (j *JsonlCanonicalWriter) Write(ars []document.Article) error {
	for _, ar := range ars {
		if err := j.enc.Encode(ar); err != nil {
			return err
		}
	}
	return nil
}

func (j *JsonlCanonicalWriter) Close() error {
	return nil
}

// parquetArticle columns use the JSON keys, so one direct mapper reads both formats.
type parquetArticle struct {
	ID              string `parquet:"id"`
	URL             string `parquet:"url"`
	Title           string `parquet:"title"`
	Description     string `parquet:"description,optional"`
	Content         string `parquet:"content"`
	PublishedAt     string `parquet:"publishedAt,optional"`
	Language        string `parquet:"language"`
	PublisherDomain string `parquet:"publisherDomain,optional"`
	Metadata        string `parquet:"metadata,optional"`
}

func toParquetArticle(ar document.Article) (parquetArticle, error) {
	row := parquetArticle{
		ID:              ar.ID.String(),
		URL:             ar.URL,
		Title:           ar.Title,
		Description:     ar.Description,
		Content:         ar.Content,
		Language:        string(ar.Language),
		PublisherDomain: ar.PublisherDomain,
	}
	if !ar.PublishedAt.IsZero() {
		row.PublishedAt = ar.PublishedAt.Format(time.RFC3339)
	}
	metadata, err := json.Marshal(ar.Metadata)
	if err != nil {
		return parquetArticle{}, fmt.Errorf("marshal metadata of article %s: %w", ar.ID, err)
	}
	row.Metadata = string(metadata)
	return row, nil
}

// A fixed row group size keeps the parquet bytes deterministic.
const canonicalRowGroupRows = 10_000

type ParquetCanonicalWriter struct {
	w *parquet.GenericWriter[parquetArticle]
}

func NewParquetCanonicalWriter(writer io.Writer) *ParquetCanonicalWriter {
	return &ParquetCanonicalWriter{
		w: parquet.NewGenericWriter[parquetArticle](writer, parquet.MaxRowsPerRowGroup(canonicalRowGroupRows)),
	}
}

func (p *ParquetCanonicalWriter) Write(ars []document.Article) error {
	rows := make([]parquetArticle, len(ars))
	for i, ar := range ars {
		row, err := toParquetArticle(ar)
		if err != nil {
			return err
		}
		rows[i] = row
	}
	_, err := p.w.Write(rows)
	return err
}

func (p *ParquetCanonicalWriter) Close() error {
	return p.w.Close()
}
