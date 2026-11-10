package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/tadjov/tusker/internal/datapackage"
	"github.com/tadjov/tusker/pkg/schema"
)

const dataPackageSchemaFile = "datapackage-v1.json"

// dataPackageSchema is closed so a misspelled key fails validation.
func dataPackageSchema() ([]byte, error) {
	generator := schema.NewGeneratorWithConfig(schema.GeneratorConfig{
		StrictValidation:     true,
		ClosedObjects:        true,
		RequireUnlessOmitted: true,
	})
	descriptor, err := generator.GenerateSchema(reflect.TypeFor[datapackage.Package]())
	if err != nil {
		return nil, fmt.Errorf("generate data package schema: %w", err)
	}
	descriptor.ID = "https://schemas.tusker.sh/datapackage-v1"
	descriptor.Title = "Data package descriptor"
	generated, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode data package schema: %w", err)
	}
	return append(generated, '\n'), nil
}

func writeDataPackageSchema(outputDir string) (string, error) {
	generated, err := dataPackageSchema()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", outputDir, err)
	}
	path := filepath.Join(outputDir, dataPackageSchemaFile)
	if err := os.WriteFile(path, generated, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
