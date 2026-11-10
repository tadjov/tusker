package judgment

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tadjov/tusker/internal/bench/meta"
	"github.com/tadjov/tusker/internal/bench/pool"
	"github.com/tadjov/tusker/internal/bench/version"
	"gopkg.in/yaml.v3"
)

// WriteFile writes a JudgmentFile atomically: marshals to a tmp file in the
// same directory, fsyncs, then renames. Prevents readers from seeing a
// half-written YAML if the process is killed mid-write. Stamps schema_version
// and the grade counts on every write so reloads always see a current artifact.
func WriteFile(jf *File, path string) error {
	jf.SchemaVersion = version.SchemaVersion
	graded, unjudged := jf.GradeCounts()
	jf.Meta.GradedCount, jf.Meta.UnjudgedCount = &graded, &unjudged
	data, err := yaml.Marshal(jf)
	if err != nil {
		return fmt.Errorf("marshal judgment file: %w", err)
	}
	return writeAtomic(path, data)
}

func ReadFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read judgment file: %w", err)
	}
	var jf File
	if err := yaml.Unmarshal(data, &jf); err != nil {
		return nil, fmt.Errorf("parse judgment file: %w", err)
	}
	if err := version.CheckSchema(jf.SchemaVersion, "annotations"); err != nil {
		return nil, err
	}
	// Fold pre-judge-block artifacts into the current shape so callers only
	// ever read Meta.Judge.
	jf.Meta.Normalize()
	return &jf, nil
}

// ReadFileIfExists is like ReadFile but returns (nil, nil) when the file is
// missing — useful for resume flows where the prior output may not exist yet.
func ReadFileIfExists(path string) (*File, error) {
	jf, err := ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return jf, err
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".bench-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}

// IncrementalWriter persists a JudgmentFile after every query so the user can
// resume after a crash, Ctrl-C, or rate-limit interrupt. Pass Append on the
// Runner's Sink.
type IncrementalWriter struct {
	Path    string
	stamp   meta.Meta
	current File
}

// NewIncrementalWriter writes stamp as the meta block of every flush, so a run
// interrupted mid-grade still leaves an artifact that names its grader and
// whatever else the caller set, which is what the resume guard reads back.
func NewIncrementalWriter(path string, stamp meta.Meta) *IncrementalWriter {
	return &IncrementalWriter{
		Path:    path,
		stamp:   stamp,
		current: File{Queries: []Entry{}},
	}
}

// LoadPrior reads the existing output file (if any) and seeds the writer with
// its queries that are still in pf. Returns the whole prior JudgmentFile so the
// caller can pass it as Runner.Existing. The prior file's meta is not carried
// over; later flushes write the stamp.
func (w *IncrementalWriter) LoadPrior(pf *pool.PoolFile) (*File, error) {
	jf, err := ReadFileIfExists(w.Path)
	if err != nil {
		return nil, err
	}
	if jf == nil {
		return nil, nil
	}
	pooled := make(map[string]bool, len(pf.Queries))
	for _, entry := range pf.Queries {
		pooled[entry.QueryID] = true
	}
	w.current = File{Queries: []Entry{}}
	for _, qe := range jf.Queries {
		if pooled[qe.QueryID] {
			w.current.Queries = append(w.current.Queries, qe)
		}
	}
	return jf, nil
}

// Append adds (or updates) a query's judgments and flushes to disk atomically.
func (w *IncrementalWriter) Append(_ QueryProgress, entry Entry) error {
	for i, qe := range w.current.Queries {
		if qe.QueryID == entry.QueryID {
			w.current.Queries[i] = entry
			return w.flush()
		}
	}
	w.current.Queries = append(w.current.Queries, entry)
	return w.flush()
}

func (w *IncrementalWriter) flush() error {
	w.current.Meta = w.stamp
	return WriteFile(&w.current, w.Path)
}
