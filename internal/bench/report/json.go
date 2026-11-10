package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tadjov/tusker/internal/bench/version"
)

func WriteJSON(r *Report, path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

func ReadJSON(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read report %s: %w", path, err)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse report %s: %w", path, err)
	}
	if err := version.CheckSchema(r.SchemaVersion, "report"); err != nil {
		return nil, fmt.Errorf("read report %s: %w", path, err)
	}
	return &r, nil
}

// latestPointer is the JSON envelope written by bench run to latest.json.
type latestPointer struct {
	Latest string `json:"latest"`
}

// ReadLatestReport follows the pointer written at latestPath (e.g.
// tracks/global-news-dataset/fts_quality/reports/latest.json) to the actual report file and
// returns the parsed Report. The pointer stores a path relative to its own
// directory so it survives moves of the track folder.
func ReadLatestReport(latestPath string) (*Report, error) {
	data, err := os.ReadFile(latestPath)
	if err != nil {
		return nil, fmt.Errorf("read latest pointer %s: %w", latestPath, err)
	}
	var ptr latestPointer
	if err := json.Unmarshal(data, &ptr); err != nil {
		return nil, fmt.Errorf("parse latest pointer %s: %w", latestPath, err)
	}
	if ptr.Latest == "" {
		return nil, fmt.Errorf("latest pointer %s has no 'latest' field", latestPath)
	}
	// Resolve relative paths against the pointer file's directory.
	reportPath := ptr.Latest
	if !filepath.IsAbs(reportPath) {
		reportPath = filepath.Join(filepath.Dir(latestPath), reportPath)
	}
	return ReadJSON(reportPath)
}
