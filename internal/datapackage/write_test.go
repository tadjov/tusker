package datapackage_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tadjov/tusker/internal/datapackage"
	"github.com/tadjov/tusker/internal/types/language"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readDescriptorJSON(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, datapackage.DescriptorName))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestWriteRecordsHashAndBytesOfEachResource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "data/hello.txt", "hello\n")
	writeFile(t, dir, "articles.csv", "id,title\n1,one\n")

	_, err := datapackage.Write(context.Background(), dir, datapackage.Package{
		Name: "fixture",
		Resources: []datapackage.Resource{
			{Name: "hello", Path: "data/hello.txt", Format: "txt"},
			{Name: "articles", Path: "articles.csv", Format: "csv"},
		},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	want := map[string]struct {
		hash  string
		bytes float64
	}{
		"hello":    {"sha256:5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03", 6},
		"articles": {"sha256:2d0edef8b70bf82650502ae43740637a3a0c5270a8ddca0d48f161cd74d6bf98", 15},
	}
	resources, _ := readDescriptorJSON(t, dir)["resources"].([]any)
	if len(resources) != len(want) {
		t.Fatalf("descriptor lists %d resources, want %d", len(resources), len(want))
	}
	for _, r := range resources {
		res := r.(map[string]any)
		name, _ := res["name"].(string)
		w, ok := want[name]
		if !ok {
			t.Fatalf("unexpected resource %q", name)
		}
		if res["hash"] != w.hash || res["bytes"] != w.bytes {
			t.Errorf("resource %s: hash=%v bytes=%v, want %s %v", name, res["hash"], res["bytes"], w.hash, w.bytes)
		}
	}
}

func helloPackage() datapackage.Package {
	return datapackage.Package{
		Name:      "fixture",
		Resources: []datapackage.Resource{{Name: "hello", Path: "hello.txt", Format: "txt"}},
	}
}

func TestWriteRejectsAnInvalidPackageWithoutWritingIt(t *testing.T) {
	resource := func(name, path, format string) datapackage.Resource {
		return datapackage.Resource{Name: name, Path: path, Format: format}
	}
	root := t.TempDir()
	writeFile(t, root, "hello.txt", "hello\n")
	tests := []struct {
		name        string
		resources   []datapackage.Resource
		packageName string
	}{
		{"no resources", nil, "fixture"},
		{"missing package name", []datapackage.Resource{resource("hello", "hello.txt", "txt")}, ""},
		{"parent directory", []datapackage.Resource{resource("hello", "../hello.txt", "txt")}, "fixture"},
		{"parent directory inside", []datapackage.Resource{resource("hello", "sub/../../hello.txt", "txt")}, "fixture"},
		{"absolute path", []datapackage.Resource{resource("hello", filepath.ToSlash(filepath.Join(root, "hello.txt")), "txt")}, "fixture"},
		{"dot prefix", []datapackage.Resource{resource("hello", "./hello.txt", "txt")}, "fixture"},
		{"remote path", []datapackage.Resource{resource("hello", "https://example.com/hello.txt", "txt")}, "fixture"},
		{"backslash", []datapackage.Resource{resource("hello", `sub\hello.txt`, "txt")}, "fixture"},
		{"missing name", []datapackage.Resource{resource("", "hello.txt", "txt")}, "fixture"},
		{"missing format", []datapackage.Resource{resource("hello", "hello.txt", "")}, "fixture"},
		{"duplicate name", []datapackage.Resource{resource("hello", "hello.txt", "txt"), resource("hello", "sub/hello.txt", "txt")}, "fixture"},
		{"missing file", []datapackage.Resource{resource("absent", "absent.txt", "txt")}, "fixture"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(root, strings.ReplaceAll(tt.name, " ", "-"), "package")
			writeFile(t, dir, "hello.txt", "hello\n")
			writeFile(t, dir, "sub/hello.txt", "hello\n")
			writeFile(t, dir, `sub\hello.txt`, "hello\n")
			writeFile(t, filepath.Dir(dir), "hello.txt", "hello\n")

			_, err := datapackage.Write(context.Background(), dir, datapackage.Package{Name: tt.packageName, Resources: tt.resources})
			if err == nil {
				t.Fatal("Write accepted an invalid package")
			}
			if _, statErr := os.Stat(filepath.Join(dir, datapackage.DescriptorName)); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("a descriptor was written despite %v", err)
			}
		})
	}
}

func TestWriteStopsOnCancelWithoutADescriptor(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hello.txt", "hello\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := datapackage.Write(ctx, dir, helloPackage())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, datapackage.DescriptorName)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Error("a descriptor was written after cancel")
	}
}

func TestWriteRejectsAnUnknownIDStrategyKind(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hello.txt", "hello\n")
	pkg := helloPackage()
	pkg.IDStrategy = &datapackage.IDStrategy{Kind: "uuidv7-typo", Source: "url"}

	if _, err := datapackage.Write(context.Background(), dir, pkg); err == nil {
		t.Fatal("Write accepted id strategy kind uuidv7-typo")
	}
	if _, err := os.Stat(filepath.Join(dir, datapackage.DescriptorName)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a descriptor was written")
	}
}

func TestWrittenDescriptorIsReadableByEveryone(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hello.txt", "hello\n")
	if _, err := datapackage.Write(context.Background(), dir, helloPackage()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, datapackage.DescriptorName))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("descriptor mode = %o, want 644", got)
	}
}

func TestWriteAndOpenAcceptOnlyAnISO6393Language(t *testing.T) {
	const helloHash = "sha256:5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	tests := []struct {
		language string
		valid    bool
	}{
		{language: "eng", valid: true},
		{language: "", valid: true},
		{language: "en"},
		{language: "english"},
	}
	for _, tt := range tests {
		t.Run("language "+tt.language, func(t *testing.T) {
			written := t.TempDir()
			writeFile(t, written, "hello.txt", "hello\n")
			pkg := helloPackage()
			pkg.Language = language.Code(tt.language)

			_, writeErr := datapackage.Write(context.Background(), written, pkg)
			if tt.valid != (writeErr == nil) {
				t.Errorf("Write with language %q: err = %v, want valid = %v", tt.language, writeErr, tt.valid)
			}

			handWritten := t.TempDir()
			writeFile(t, handWritten, "hello.txt", "hello\n")
			writeFile(t, handWritten, datapackage.DescriptorName, `{"name": "fixture", "language": "`+tt.language+`",
				"resources": [{"name": "hello", "path": "hello.txt", "format": "txt", "bytes": 6, "hash": "`+helloHash+`"}]}`)

			opened, openErr := datapackage.Open(context.Background(), handWritten)
			if tt.valid != (openErr == nil) {
				t.Fatalf("Open with language %q: err = %v, want valid = %v", tt.language, openErr, tt.valid)
			}
			if tt.valid && opened.Package.Language != language.Code(tt.language) {
				t.Errorf("opened language = %q, want %q", opened.Package.Language, tt.language)
			}
		})
	}
}
