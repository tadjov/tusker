package datapackage_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/tadjov/tusker/internal/datapackage"
)

const (
	helloHash      = "sha256:5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	helloAgainHash = "sha256:aeac3c7989e787af3f62a1b932c47ac6afeaa79cf3281caf8a328ee055071fed"
)

func stagedArticles(t *testing.T, root, content, tool string) (staging string, pkg datapackage.Package) {
	t.Helper()
	staging, err := os.MkdirTemp(root, "staging-")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, staging, "articles.jsonl", content)
	return staging, datapackage.Package{
		Name:       "cc-news-canonical",
		ProducedBy: &datapackage.ProducedBy{Tool: tool},
		Resources:  []datapackage.Resource{{Name: datapackage.ArticlesResource, Path: "articles.jsonl", Format: "jsonl"}},
	}
}

func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s still exists", path)
	}
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestPublishMovesTheStagedPackageToTheCallersVersion(t *testing.T) {
	tests := []struct {
		name, version, want string
	}{
		{"named version", "v2", "v2"},
		{"default version", "", "v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "canonical")
			staging, pkg := stagedArticles(t, root, "hello\n", "datapipe preprocess")

			published, err := datapackage.Publish(context.Background(), pkg, datapackage.PublishOptions{
				StagingDir: staging, ParentDir: parent, Version: tt.version,
			})
			if err != nil {
				t.Fatalf("Publish: %v", err)
			}

			if want := filepath.Join(parent, tt.want); published.Dir != want || published.Package.Version != tt.want || published.Existing {
				t.Errorf("published = %s version %q existing %v, want %s version %s, new",
					published.Dir, published.Package.Version, published.Existing, want, tt.want)
			}
			absent(t, staging)
			opened, err := datapackage.Open(context.Background(), published.Dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if got, err := readResource(context.Background(), opened, datapackage.ArticlesResource); err != nil || string(got) != "hello\n" {
				t.Errorf("articles = %q, %v", got, err)
			}
		})
	}
}

func TestPublishKeepsTheSameBytesAtAnExistingVersion(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "canonical")
	ctx := context.Background()
	first, pkg := stagedArticles(t, root, "hello\n", "datapipe preprocess")
	published, err := datapackage.Publish(ctx, pkg, datapackage.PublishOptions{StagingDir: first, ParentDir: parent})
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	before := readFileBytes(t, filepath.Join(published.Dir, datapackage.DescriptorName))

	second, rerun := stagedArticles(t, root, "hello\n", "datapipe preprocess, rerun")
	again, err := datapackage.Publish(ctx, rerun, datapackage.PublishOptions{StagingDir: second, ParentDir: parent})
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	if !again.Existing || again.Dir != published.Dir || again.Package.ProducedBy.Tool != "datapipe preprocess" {
		t.Errorf("rerun = %s existing %v tool %q, want the existing %s from the first run",
			again.Dir, again.Existing, again.Package.ProducedBy.Tool, published.Dir)
	}
	absent(t, second)
	if after := readFileBytes(t, filepath.Join(published.Dir, datapackage.DescriptorName)); !bytes.Equal(before, after) {
		t.Errorf("the published descriptor changed:\n%s", after)
	}
}

func TestPublishRefusesDifferentBytesAtAnExistingVersion(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "canonical")
	ctx := context.Background()
	first, pkg := stagedArticles(t, root, "hello\n", "datapipe preprocess")
	published, err := datapackage.Publish(ctx, pkg, datapackage.PublishOptions{StagingDir: first, ParentDir: parent})
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	descriptor := readFileBytes(t, filepath.Join(published.Dir, datapackage.DescriptorName))

	second, changed := stagedArticles(t, root, "hello, again\n", "datapipe preprocess")
	_, err = datapackage.Publish(ctx, changed, datapackage.PublishOptions{StagingDir: second, ParentDir: parent})

	var conflict *datapackage.VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want VersionConflictError", err)
	}
	if conflict.Version != "v1" || conflict.PublishedHash != helloHash || conflict.StagedHash != helloAgainHash {
		t.Errorf("conflict = %+v, want v1 published %s staged %s", conflict, helloHash, helloAgainHash)
	}
	if got := readFileBytes(t, filepath.Join(published.Dir, datapackage.DescriptorName)); !bytes.Equal(got, descriptor) {
		t.Errorf("the published descriptor changed:\n%s", got)
	}
	if got := readFileBytes(t, filepath.Join(published.Dir, "articles.jsonl")); string(got) != "hello\n" {
		t.Errorf("the published resource is now %q", got)
	}
	absent(t, second)
}

func TestPublishRefusesWhatItCannotPublish(t *testing.T) {
	canonical := func(staging string) string { return filepath.Join(filepath.Dir(staging), "canonical") }
	tests := map[string]func(staging string, pkg *datapackage.Package) datapackage.PublishOptions{
		"two resources": func(staging string, pkg *datapackage.Package) datapackage.PublishOptions {
			writeFile(t, staging, "more.jsonl", "more\n")
			pkg.Resources = append(pkg.Resources, datapackage.Resource{Name: "more", Path: "more.jsonl", Format: "jsonl"})
			return datapackage.PublishOptions{StagingDir: staging, ParentDir: canonical(staging)}
		},
		"a version on the package": func(staging string, pkg *datapackage.Package) datapackage.PublishOptions {
			pkg.Version = "v1"
			return datapackage.PublishOptions{StagingDir: staging, ParentDir: canonical(staging)}
		},
		"a version that is not one folder name": func(staging string, _ *datapackage.Package) datapackage.PublishOptions {
			return datapackage.PublishOptions{StagingDir: staging, ParentDir: canonical(staging), Version: "../v1"}
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			staging, pkg := stagedArticles(t, t.TempDir(), "hello\n", "datapipe preprocess")
			opts := setup(staging, &pkg)

			if _, err := datapackage.Publish(context.Background(), pkg, opts); err == nil {
				t.Fatal("Publish succeeded")
			}
			if _, err := os.Stat(filepath.Join(staging, "articles.jsonl")); err != nil {
				t.Errorf("the staged resource is gone: %v", err)
			}
			absent(t, filepath.Join(opts.ParentDir, "v1"))
		})
	}
}

func TestPublishStopsOnCancelWithoutPublishing(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "canonical")
	staging, pkg := stagedArticles(t, root, "hello\n", "datapipe preprocess")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := datapackage.Publish(ctx, pkg, datapackage.PublishOptions{StagingDir: staging, ParentDir: parent})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	absent(t, filepath.Join(parent, "v1"))
}

// Discarding such a staging folder would delete the package published inside it.
func TestPublishRefusesAParentInsideTheStagingFolder(t *testing.T) {
	ctx := context.Background()
	staging, pkg := stagedArticles(t, t.TempDir(), "hello\n", "datapipe preprocess")
	parent := filepath.Join(staging, "canonical")
	earlier, earlierPkg := stagedArticles(t, t.TempDir(), "hello\n", "datapipe preprocess")
	published, err := datapackage.Publish(ctx, earlierPkg, datapackage.PublishOptions{StagingDir: earlier, ParentDir: parent})
	if err != nil {
		t.Fatalf("earlier Publish: %v", err)
	}

	if _, err := datapackage.Publish(ctx, pkg, datapackage.PublishOptions{StagingDir: staging, ParentDir: parent}); err == nil {
		t.Fatal("Publish succeeded with the parent inside the staging folder")
	}
	if _, err := os.Stat(filepath.Join(published.Dir, datapackage.DescriptorName)); err != nil {
		t.Errorf("the package published earlier is gone: %v", err)
	}
}
