// Package queryvectors serves a bench track's precomputed query vectors.
package queryvectors

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/parquet-go/parquet-go"
	"github.com/tadjov/tusker/internal/bench/suite"
	"github.com/tadjov/tusker/internal/datapackage"
	"github.com/tadjov/tusker/internal/storage/pg"
)

type Options struct {
	// Package is the package directory or descriptor location.
	Package string
	// Suite is the suite.yaml path.
	Suite string
	// Model is the model@dim tag the package must carry.
	Model string
	// Postgres serves document vectors; nil leaves DocVectors refusing.
	Postgres *pg.ConnectionPool
}

// Store serves query vectors from the package and document vectors from Postgres.
type Store struct {
	location string
	tag      string
	vectors  map[string][]float32
	docs     *pg.VectorStore
}

// UnknownQueryError reports a query text the package holds no vector for.
type UnknownQueryError struct {
	Package string
	Text    string
}

func (e *UnknownQueryError) Error() string {
	return fmt.Sprintf("query-vectors package %s has no vector for query text %q", e.Package, e.Text)
}

// StaleQueryError reports a suite query whose vector is missing or outdated.
type StaleQueryError struct {
	Package string
	QueryID string
	// Text is the query's text in the current suite.
	Text string
	// Stored is the text the package embedded; nil when it has no row for QueryID.
	Stored *string
}

func (e *StaleQueryError) Error() string {
	if e.Stored == nil {
		return fmt.Sprintf("query-vectors package %s has no row for suite query %s: re-embed the queries",
			e.Package, e.QueryID)
	}
	return fmt.Sprintf("query-vectors package %s embedded suite query %s as %q, but the suite now reads %q: re-embed the queries",
		e.Package, e.QueryID, *e.Stored, e.Text)
}

type row struct {
	QueryID string `parquet:"query_id"`
	Text    string `parquet:"text"`
	// `,list` matches the 3-level LIST encoding pyarrow writes.
	Embedding []float32 `parquet:"embedding,list"`
}

// Open returns *StaleQueryError when the suite and package disagree.
func Open(ctx context.Context, opts Options) (*Store, error) {
	opened, err := datapackage.Open(ctx, opts.Package)
	if err != nil {
		return nil, fmt.Errorf("open query-vectors package: %w", err)
	}
	pkg := opened.Package
	if pkg.Embedding == nil {
		return nil, fmt.Errorf("query-vectors package %s records no embedding", opened.Location)
	}
	if pkg.Suite == nil {
		return nil, fmt.Errorf("query-vectors package %s records no suite", opened.Location)
	}
	if err := checkEmbedding(opened, opts.Model); err != nil {
		return nil, err
	}
	rows, err := readRows(ctx, opened)
	if err != nil {
		return nil, err
	}
	if err := checkSuiteQueries(opened.Location, rows, opts.Suite); err != nil {
		return nil, err
	}
	store := &Store{location: opened.Location, tag: pkg.Embedding.Tag, vectors: rows.vectorByText}
	if opts.Postgres != nil {
		store.docs = pg.NewVectorStore(nil, opts.Postgres, store.tag)
	}
	return store, nil
}

// checkSuiteQueries compares each query's text, the string engines and judges
// look vectors up by.
func checkSuiteQueries(location string, rows packageRows, suitePath string) error {
	loaded, err := suite.LoadFromFile(suitePath)
	if err != nil {
		return fmt.Errorf("load suite %s to check query vectors: %w", suitePath, err)
	}
	for _, q := range loaded.Suite.Queries {
		stored, ok := rows.textByQueryID[q.ID]
		switch {
		case !ok:
			return &StaleQueryError{Package: location, QueryID: q.ID, Text: q.Text}
		case stored != q.Text:
			return &StaleQueryError{Package: location, QueryID: q.ID, Text: q.Text, Stored: &stored}
		}
	}
	return nil
}

// QueryRecipe is the only query recipe the bench embeds with.
const QueryRecipe = "query-instruct-v1"

func checkEmbedding(opened *datapackage.Opened, declared string) error {
	embedding := opened.Package.Embedding
	if declared == "" {
		return fmt.Errorf("query-vectors package %s is tagged %s, but the spec declares no embedding_model to check it against",
			opened.Location, embedding.Tag)
	}
	if embedding.Tag != declared {
		return fmt.Errorf("query-vectors package %s is tagged %s, but the spec declares embedding_model %s",
			opened.Location, embedding.Tag, declared)
	}
	if embedding.Recipe != QueryRecipe {
		return fmt.Errorf("query-vectors package %s was embedded with recipe %q, want %q",
			opened.Location, embedding.Recipe, QueryRecipe)
	}
	return nil
}

type packageRows struct {
	vectorByText  map[string][]float32
	textByQueryID map[string]string
}

func readRows(ctx context.Context, opened *datapackage.Opened) (packageRows, error) {
	resource, err := opened.Resource(ctx, datapackage.QueryVectorsResource)
	if err != nil {
		return packageRows{}, fmt.Errorf("read query vectors of %s: %w", opened.Location, err)
	}
	defer resource.Close()
	file, err := parquet.OpenFile(resource, resource.Size())
	if err != nil {
		return packageRows{}, fmt.Errorf("parse query vectors of %s: %w", opened.Location, err)
	}
	reader := parquet.NewGenericReader[row](file)
	defer reader.Close()

	dim := opened.Package.Embedding.Dim

	rows := packageRows{vectorByText: make(map[string][]float32), textByQueryID: make(map[string]string)}
	for {
		batch := make([]row, 64)
		n, err := reader.Read(batch)
		for _, r := range batch[:n] {
			if len(r.Embedding) != dim {
				return packageRows{}, fmt.Errorf("query %s in %s has %d dimensions, the package declares %d",
					r.QueryID, opened.Location, len(r.Embedding), dim)
			}
			rows.vectorByText[r.Text] = r.Embedding
			rows.textByQueryID[r.QueryID] = r.Text
		}
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return packageRows{}, fmt.Errorf("read query vectors of %s: %w", opened.Location, err)
		}
	}
}

func (s *Store) QueryVector(_ context.Context, text string) ([]float32, error) {
	vector, ok := s.vectors[text]
	if !ok {
		return nil, &UnknownQueryError{Package: s.location, Text: text}
	}
	return vector, nil
}

func (s *Store) DocVectors(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]float32, error) {
	if s.docs == nil {
		return nil, errors.New("query-vectors store has no Postgres to read document vectors from")
	}
	return s.docs.DocVectors(ctx, ids)
}

func (s *Store) Model() string { return s.tag }
