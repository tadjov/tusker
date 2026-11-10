package main

import (
	"os"
	"strconv"
	"strings"

	"github.com/tadjov/tusker/internal/cli"
	"github.com/tadjov/tusker/internal/ingest/reader"
)

// rowCounter is implemented by readers whose format records its row count in
// metadata. CSV and JSONL do not: counting would mean a second full pass over
// the file to print a number the summary reports anyway.
type rowCounter interface {
	NumRows() int64
}

// datasetFields describes the input a command is about to read. reader may be
// nil when the command has not opened one.
func datasetFields(path string, source reader.RawParallelReader) []cli.Field {
	fields := []cli.Field{
		{Label: "source", Value: path},
		{Label: "format", Value: strings.TrimPrefix(string(fileExt(path)), ".")},
	}
	if info, err := os.Stat(path); err == nil {
		fields = append(fields, cli.ByteField("size", info.Size()))
	}
	if counter, ok := source.(rowCounter); ok {
		fields = append(fields, cli.Field{Label: "rows", Value: strconv.FormatInt(counter.NumRows(), 10)})
	}
	return fields
}

var _ rowCounter = (*reader.ParquetReader)(nil)
