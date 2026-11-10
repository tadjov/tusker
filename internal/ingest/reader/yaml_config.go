package reader

import (
	"fmt"
	"io"

	"github.com/tadjov/tusker/pkg/apis/datamapping"
	"gopkg.in/yaml.v3"
)

type YAMLConfigLoader struct {
	reader io.Reader
}

func NewYAMLConfigLoader(reader io.Reader) *YAMLConfigLoader {
	return &YAMLConfigLoader{
		reader: reader,
	}
}

// Load fails on any key the schema does not define.
func (cl *YAMLConfigLoader) Load(validate bool) (*datamapping.DataMapper, error) {
	decoder := yaml.NewDecoder(cl.reader)
	decoder.KnownFields(true)
	var mapping datamapping.DataMapper
	if err := decoder.Decode(&mapping); err != nil {
		return nil, fmt.Errorf("decode mapping: %w", err)
	}
	if validate {
		if err := mapping.Validate(); err != nil {
			return nil, err
		}
	}
	return &mapping, nil
}
