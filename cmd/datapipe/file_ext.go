package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tadjov/tusker/internal/ingest"
	"github.com/tadjov/tusker/internal/ingest/reader"
)

type FileExt string

const (
	ExtCSV     FileExt = ".csv"
	ExtJSONL   FileExt = ".jsonl"
	ExtParquet FileExt = ".parquet"
	ExtYAML    FileExt = ".yaml"
)

func fileExt(path string) FileExt {
	return FileExt(strings.ToLower(filepath.Ext(path)))
}

func validateFileExt(path string, supported map[FileExt]struct{}) error {
	ext := fileExt(path)

	if _, ok := supported[ext]; !ok {
		return fmt.Errorf("unsupported file extension %q, want one of %s", ext, listFileExts(supported))
	}

	return nil
}

func listFileExts(supported map[FileExt]struct{}) string {
	exts := make([]string, 0, len(supported))
	for ext := range supported {
		exts = append(exts, string(ext))
	}
	slices.Sort(exts)
	return strings.Join(exts, ", ")
}

// OpenDatasetReader opens the dataset at path and returns a reader for its
// format. The caller closes the returned file.
func OpenDatasetReader(path string) (reader.RawParallelReader, io.Closer, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open dataset: %w", err)
	}

	fail := func(err error) (reader.RawParallelReader, io.Closer, error) {
		file.Close()
		return nil, nil, err
	}

	switch ext := fileExt(path); ext {
	case ExtCSV:
		return reader.NewCSVReader(file), file, nil
	case ExtJSONL:
		return reader.NewJSONLReader(file), file, nil
	case ExtParquet:
		info, err := file.Stat()
		if err != nil {
			return fail(fmt.Errorf("stat dataset: %w", err))
		}
		parquet, err := reader.NewParquetReader(file, info.Size())
		if err != nil {
			return fail(err)
		}
		return parquet, file, nil
	default:
		return fail(fmt.Errorf("unknown input format: %s", ext))
	}
}

func NewCanonicalWriter(w io.Writer, ext FileExt) (ingest.CanonicalWriter, error) {
	switch ext {
	case ExtJSONL:
		return ingest.NewJsonlCanonicalWriter(w), nil
	case ExtParquet:
		return ingest.NewParquetCanonicalWriter(w), nil
	default:
		return nil, fmt.Errorf("unknown output format: %s", ext)
	}
}
