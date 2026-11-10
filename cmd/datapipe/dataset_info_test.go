package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/cli"
	"github.com/tadjov/tusker/internal/ingest/reader"
)

type plainSource struct{ reader.RawParallelReader }

type countingSource struct {
	reader.RawParallelReader
	rows int64
}

func (c countingSource) NumRows() int64 { return c.rows }

func fieldValue(fields []cli.Field, label string) (string, bool) {
	for _, f := range fields {
		if f.Label == label {
			return f.Value, true
		}
	}
	return "", false
}

func TestDatasetFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))

	t.Run("reader without a row count omits rows", func(t *testing.T) {
		fields := datasetFields(path, plainSource{})

		_, ok := fieldValue(fields, "rows")
		assert.False(t, ok, "counting csv/jsonl would mean a second pass over the file")

		format, _ := fieldValue(fields, "format")
		assert.Equal(t, "jsonl", format, "the extension's dot is noise in a rendered label")
	})

	t.Run("reader that knows its row count reports it", func(t *testing.T) {
		fields := datasetFields(path, countingSource{rows: 708241})

		rows, ok := fieldValue(fields, "rows")
		require.True(t, ok)
		assert.Equal(t, "708241", rows)
	})

	t.Run("a missing file still describes what it can", func(t *testing.T) {
		fields := datasetFields(filepath.Join(dir, "gone.csv"), nil)

		_, ok := fieldValue(fields, "size")
		assert.False(t, ok)
		source, _ := fieldValue(fields, "source")
		assert.Contains(t, source, "gone.csv")
	})
}
