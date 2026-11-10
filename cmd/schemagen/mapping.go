package main

import (
	"fmt"

	"github.com/tadjov/tusker/pkg/apis/datamapping"
	"github.com/tadjov/tusker/pkg/schema"
)

const mappingSchemaFile = "datamapping-v1.json"

// mappingSchema is closed because the loader rejects unknown keys too.
func mappingSchema(strict bool) ([]byte, error) {
	generator := schema.NewGeneratorWithConfig(schema.GeneratorConfig{
		StrictValidation: strict,
		ClosedObjects:    true,
	})
	generated, err := generator.GenerateJSONSchema(datamapping.DataMapper{})
	if err != nil {
		return nil, fmt.Errorf("generate data mapping schema: %w", err)
	}
	return []byte(generated), nil
}
