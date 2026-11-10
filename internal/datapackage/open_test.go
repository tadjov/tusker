package datapackage_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tadjov/tusker/internal/datapackage"
)

func writeFullPackage(t *testing.T, dir string) datapackage.Package {
	t.Helper()
	writeFile(t, dir, "articles.parquet", "not really parquet")
	writeFile(t, dir, "embeddings.parquet", "not really vectors")
	written, err := datapackage.Write(context.Background(), dir, fullPackage())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return written
}

func TestOpenReturnsTheWrittenDescriptor(t *testing.T) {
	sparse := fullPackage()
	sparse.Report = &datapackage.Report{Read: 3, Written: 3, Dropped: map[string]int64{}}
	sparse.Sources = []datapackage.Source{}
	packages := map[string]datapackage.Package{"every property": fullPackage(), "empty collections": sparse}

	for name, pkg := range packages {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "articles.parquet", "not really parquet")
			writeFile(t, dir, "embeddings.parquet", "not really vectors")
			written, err := datapackage.Write(context.Background(), dir, pkg)
			if err != nil {
				t.Fatalf("Write: %v", err)
			}

			for _, location := range []string{dir, filepath.Join(dir, datapackage.DescriptorName)} {
				opened, err := datapackage.Open(context.Background(), location)
				if err != nil {
					t.Fatalf("Open(%s): %v", location, err)
				}
				if !reflect.DeepEqual(opened.Package, written) {
					t.Errorf("Open(%s) descriptor differs:\n got %#v\nwant %#v", location, opened.Package, written)
				}
			}
		})
	}
}

func readResource(ctx context.Context, opened *datapackage.Opened, name string) ([]byte, error) {
	r, err := opened.Resource(ctx, name)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func TestResourceYieldsItsBytesSequentiallyAndAtOffsets(t *testing.T) {
	dir := t.TempDir()
	writeFullPackage(t, dir)
	opened, err := datapackage.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	r, err := opened.Resource(context.Background(), "embeddings")
	if err != nil {
		t.Fatalf("Resource: %v", err)
	}
	defer r.Close()
	if r.Size() != int64(len("not really vectors")) {
		t.Errorf("Size() = %d, want %d", r.Size(), len("not really vectors"))
	}
	tail := make([]byte, len("vectors"))
	if _, err := r.ReadAt(tail, int64(len("not really "))); err != nil || string(tail) != "vectors" {
		t.Errorf("ReadAt = %q, %v, want %q", tail, err, "vectors")
	}
	all, err := io.ReadAll(r)
	if err != nil || string(all) != "not really vectors" {
		t.Errorf("ReadAll = %q, %v", all, err)
	}
}

func TestResourceFailsWhenItsContentNoLongerMatches(t *testing.T) {
	tests := []struct {
		name    string
		content string
		check   func(t *testing.T, err error)
	}{
		{"one byte flipped", "not really vectorS", func(t *testing.T, err error) {
			var mismatch *datapackage.HashMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("err = %v, want HashMismatchError", err)
			}
			if mismatch.Resource != "embeddings" {
				t.Errorf("mismatch names resource %q", mismatch.Resource)
			}
		}},
		{"truncated", "not really", func(t *testing.T, err error) {
			var mismatch *datapackage.ByteCountMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("err = %v, want ByteCountMismatchError", err)
			}
			if mismatch.Want != 18 || mismatch.Got != 10 {
				t.Errorf("byte counts = want %d got %d, expected 18 and 10", mismatch.Want, mismatch.Got)
			}
		}},
		{"appended", "not really vectors, and more", func(t *testing.T, err error) {
			var mismatch *datapackage.ByteCountMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("err = %v, want ByteCountMismatchError", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFullPackage(t, dir)
			opened, err := datapackage.Open(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, dir, "embeddings.parquet", tt.content)

			got, err := readResource(context.Background(), opened, "embeddings")
			if got != nil {
				t.Errorf("a mismatched resource yielded %q", got)
			}
			tt.check(t, err)
		})
	}
}

func TestResourceReportsWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	writeFullPackage(t, dir)
	opened, err := datapackage.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	_, err = opened.Resource(context.Background(), "vectors")
	var unknown *datapackage.ResourceNotFoundError
	if !errors.As(err, &unknown) || unknown.Name != "vectors" {
		t.Errorf("unknown name: err = %v, want ResourceNotFoundError for vectors", err)
	}

	if err := os.Remove(filepath.Join(dir, "articles.parquet")); err != nil {
		t.Fatal(err)
	}
	_, err = opened.Resource(context.Background(), "articles")
	var missing *datapackage.NotFoundError
	if !errors.As(err, &missing) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: err = %v, want NotFoundError wrapping fs.ErrNotExist", err)
	}
}

func TestOpenRejectsADescriptorItCannotTrust(t *testing.T) {
	const valid = `"name": "x", "path": "x.txt", "format": "txt", "bytes": 1, "hash": "sha256:` + zeros64 + `"`
	tests := map[string]string{
		"escaping path":   `{"name": "p", "resources": [{"name": "x", "path": "../secret.txt", "format": "txt", "bytes": 1, "hash": "sha256:` + zeros64 + `"}]}`,
		"absolute path":   `{"name": "p", "resources": [{"name": "x", "path": "/etc/passwd", "format": "txt", "bytes": 1, "hash": "sha256:` + zeros64 + `"}]}`,
		"md5 hash":        `{"name": "p", "resources": [{"name": "x", "path": "x.txt", "format": "txt", "bytes": 1, "hash": "md5:d41d8cd98f00b204e9800998ecf8427e"}]}`,
		"negative bytes":  `{"name": "p", "resources": [{"name": "x", "path": "x.txt", "format": "txt", "bytes": -1, "hash": "sha256:` + zeros64 + `"}]}`,
		"short hash":      `{"name": "p", "resources": [{"name": "x", "path": "x.txt", "format": "txt", "bytes": 1, "hash": "sha256:abc"}]}`,
		"no resources":    `{"name": "p", "resources": []}`,
		"not json":        `{"name": "p", "resources": [{` + valid + `}]`,
		"duplicate name":  `{"name": "p", "resources": [{` + valid + `}, {` + valid + `}]}`,
		"unknown id kind": `{"name": "p", "idStrategy": {"kind": "uuidv7-typo"}, "resources": [{` + valid + `}]}`,
	}
	for name, descriptor := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, datapackage.DescriptorName, descriptor)
			if _, err := datapackage.Open(context.Background(), dir); err == nil {
				t.Error("Open accepted the descriptor")
			}
		})
	}
}

func TestOpenReportsAMissingDescriptor(t *testing.T) {
	_, err := datapackage.Open(context.Background(), t.TempDir())
	var missing *datapackage.NotFoundError
	if !errors.As(err, &missing) {
		t.Errorf("err = %v, want NotFoundError", err)
	}
}

func TestOpenAcceptsADescriptorWithoutAPackageName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hello.txt", "hello\n")
	writeFile(t, dir, datapackage.DescriptorName, `{"resources": [{"name": "hello", "path": "hello.txt", "format": "txt", "bytes": 6,
		"hash": "sha256:5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"}]}`)

	opened, err := datapackage.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, err := readResource(context.Background(), opened, "hello"); err != nil || string(got) != "hello\n" {
		t.Errorf("hello = %q, %v", got, err)
	}
}

func TestResourceFailsWhenADescriptorClaimsZeroBytesWrongly(t *testing.T) {
	tests := []struct {
		name    string
		content string
		check   func(t *testing.T, err error)
	}{
		{"non-empty file", "hello\n", func(t *testing.T, err error) {
			var mismatch *datapackage.ByteCountMismatchError
			if !errors.As(err, &mismatch) {
				t.Errorf("err = %v, want ByteCountMismatchError", err)
			}
		}},
		{"empty file with a wrong hash", "", func(t *testing.T, err error) {
			var mismatch *datapackage.HashMismatchError
			if !errors.As(err, &mismatch) {
				t.Errorf("err = %v, want HashMismatchError", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "x.txt", tt.content)
			writeFile(t, dir, datapackage.DescriptorName, `{"name": "p", "resources": [{"name": "x", "path": "x.txt", "format": "txt", "bytes": 0, "hash": "sha256:`+zeros64+`"}]}`)
			opened, err := datapackage.Open(context.Background(), dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			got, err := readResource(context.Background(), opened, "x")
			if got != nil {
				t.Errorf("a mismatched resource yielded %q", got)
			}
			tt.check(t, err)
		})
	}
}
