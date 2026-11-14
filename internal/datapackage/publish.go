package datapackage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const DefaultVersion = "v1"

// PublishOptions locates a package being published; StagingDir and ParentDir must share a filesystem.
type PublishOptions struct {
	StagingDir string
	ParentDir  string
	Version    string // empty means DefaultVersion
}

// Published is a package in its version folder; Existing means it was already there,
// Restored that its missing resource came from this run.
type Published struct {
	Dir      string
	Package  Package
	Existing bool
	Restored bool
}

// VersionConflictError reports a version already published with other bytes.
type VersionConflictError struct {
	Dir           string
	Version       string
	PublishedHash string
	StagedHash    string
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("version %s at %s already holds %s, not the staged %s; publish under another version",
		e.Version, e.Dir, e.PublishedHash, e.StagedHash)
}

// Publish moves a staged canonical or embeddings package to ParentDir/<version>.
// An existing version with the same resource hash is kept, its resource restored if
// missing; any other is a *VersionConflictError.
func Publish(ctx context.Context, pkg Package, opts PublishOptions) (Published, error) {
	if opts.Version == "" {
		opts.Version = DefaultVersion
	}
	if err := checkPublishable(pkg, opts); err != nil {
		return Published{}, err
	}
	pkg.Version = opts.Version
	staged, err := Write(ctx, opts.StagingDir, pkg)
	if err != nil {
		return Published{}, err
	}
	target := filepath.Join(opts.ParentDir, opts.Version)
	if existing, ok, err := openPublished(ctx, target); err != nil || ok {
		if err != nil {
			return Published{}, err
		}
		return keepPublished(opts.StagingDir, existing, staged)
	}
	if err := os.MkdirAll(opts.ParentDir, 0o755); err != nil {
		return Published{}, fmt.Errorf("create %s: %w", opts.ParentDir, err)
	}
	if err := os.Rename(opts.StagingDir, target); err != nil {
		// Another producer may have published this version meanwhile.
		if existing, ok, openErr := openPublished(ctx, target); openErr == nil && ok {
			return keepPublished(opts.StagingDir, existing, staged)
		}
		return Published{}, fmt.Errorf("publish %s as %s: %w", opts.StagingDir, target, err)
	}
	return Published{Dir: target, Package: staged}, nil
}

func checkPublishable(pkg Package, opts PublishOptions) error {
	if len(pkg.Resources) != 1 {
		return fmt.Errorf("publish package %s: it must list one resource, it lists %d", pkg.Name, len(pkg.Resources))
	}
	if pkg.Version != "" {
		return fmt.Errorf("publish package %s: version %q is set on the package; pass it in PublishOptions", pkg.Name, pkg.Version)
	}
	if err := validateResourcePath(opts.Version); err != nil || strings.Contains(opts.Version, "/") {
		return fmt.Errorf("publish package %s: version %q must be a single folder name", pkg.Name, opts.Version)
	}
	if opts.StagingDir == "" || opts.ParentDir == "" {
		return errors.New("publish needs both a staging and a parent folder")
	}
	staging, err := filepath.Abs(opts.StagingDir)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", opts.StagingDir, err)
	}
	parent, err := filepath.Abs(opts.ParentDir)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", opts.ParentDir, err)
	}
	if rel, err := filepath.Rel(staging, parent); err == nil && !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("publish: parent %s lies inside staging %s, which is removed or moved", opts.ParentDir, opts.StagingDir)
	}
	return nil
}

func openPublished(ctx context.Context, dir string) (Published, bool, error) {
	if _, err := os.Stat(filepath.Join(dir, DescriptorName)); errors.Is(err, fs.ErrNotExist) {
		return Published{}, false, nil
	}
	opened, err := Open(ctx, dir)
	if err != nil {
		return Published{}, false, fmt.Errorf("open published %s: %w", dir, err)
	}
	return Published{Dir: dir, Package: opened.Package, Existing: true}, true, nil
}

func keepPublished(staging string, existing Published, staged Package) (Published, error) {
	// Open guarantees at least one resource.
	publishedHash := existing.Package.Resources[0].Hash
	if len(existing.Package.Resources) != 1 || publishedHash != staged.Resources[0].Hash {
		if err := os.RemoveAll(staging); err != nil {
			return Published{}, fmt.Errorf("remove staging %s: %w", staging, err)
		}
		return Published{}, &VersionConflictError{
			Dir: existing.Dir, Version: staged.Version,
			PublishedHash: publishedHash, StagedHash: staged.Resources[0].Hash,
		}
	}
	restored, err := restoreMissingResource(staging, existing, staged)
	if err != nil {
		return Published{}, err
	}
	if err := os.RemoveAll(staging); err != nil {
		return Published{}, fmt.Errorf("remove staging %s: %w", staging, err)
	}
	existing.Restored = restored
	return existing, nil
}

// restoreMissingResource fills in a resource whose descriptor is on disk without it,
// as in a clone that tracks descriptors but not payloads. The hashes already match.
func restoreMissingResource(staging string, existing Published, staged Package) (bool, error) {
	target := filepath.Join(existing.Dir, existing.Package.Resources[0].Path)
	if _, err := os.Stat(target); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("check published resource %s: %w", target, err)
	}
	if err := os.Rename(filepath.Join(staging, staged.Resources[0].Path), target); err != nil {
		return false, fmt.Errorf("restore published resource %s: %w", target, err)
	}
	return true, nil
}
