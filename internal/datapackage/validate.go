package datapackage

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tadjov/tusker/internal/types/language"
	"github.com/tadjov/tusker/pkg/apis/datamapping"
)

func (p Package) validate() error {
	// Embeddings and raw packages carry no corpus language.
	if p.Language != "" {
		if _, err := language.Parse(string(p.Language)); err != nil {
			return fmt.Errorf("package %s: %w", p.Name, err)
		}
	}
	if p.IDStrategy != nil {
		if _, err := datamapping.ParseIdStrategyKind(string(p.IDStrategy.Kind)); err != nil {
			return fmt.Errorf("package %s: %w", p.Name, err)
		}
	}
	if p.Suite != nil && !p.hasResource(QueryVectorsResource) {
		return fmt.Errorf("package %s: suite is set but no %s resource is listed", p.Name, QueryVectorsResource)
	}
	if len(p.Resources) == 0 {
		return fmt.Errorf("package %s has no resources", p.Name)
	}
	seen := make(map[string]bool, len(p.Resources))
	for _, r := range p.Resources {
		if r.Name == "" {
			return fmt.Errorf("package %s: resource at %q has no name", p.Name, r.Path)
		}
		if seen[r.Name] {
			return fmt.Errorf("package %s: resource name %q is used twice", p.Name, r.Name)
		}
		seen[r.Name] = true
		if r.Format == "" {
			return fmt.Errorf("package %s: resource %s has no format", p.Name, r.Name)
		}
		if err := validateResourcePath(r.Path); err != nil {
			return fmt.Errorf("package %s: resource %s: %w", p.Name, r.Name, err)
		}
	}
	return nil
}

// validateResourcePath keeps a descriptor from pointing outside its folder.
func validateResourcePath(p string) error {
	switch {
	case p == "":
		return errors.New("empty path")
	case strings.ContainsAny(p[:1], "./~"):
		return fmt.Errorf("path %q must not start with '.', '/' or '~'", p)
	case strings.Contains(p, `\`):
		return fmt.Errorf("path %q must use '/' separators", p)
	case strings.Contains(p, ":"):
		return fmt.Errorf("path %q must be relative to the descriptor", p)
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." || segment == "." || segment == "" {
			return fmt.Errorf("path %q must not contain empty, '.' or '..' segments", p)
		}
	}
	return nil
}

func (p Package) hasResource(name string) bool {
	for _, r := range p.Resources {
		if r.Name == name {
			return true
		}
	}
	return false
}
