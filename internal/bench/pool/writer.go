package pool

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tadjov/tusker/internal/bench/version"
	"gopkg.in/yaml.v3"
)

// WritePoolFile stamps schema_version then writes the file. The caller is
// expected to have populated pf.Meta via meta.New("pool") before calling.
func WritePoolFile(pf *PoolFile, path string) error {
	pf.SchemaVersion = version.SchemaVersion
	data, err := yaml.Marshal(pf)
	if err != nil {
		return fmt.Errorf("marshal pool file: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write pool file: %w", err)
	}
	return nil
}
