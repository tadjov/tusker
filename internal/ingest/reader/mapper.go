package reader

import (
	"github.com/tadjov/tusker/internal/types/document"
	"github.com/tadjov/tusker/pkg/apis/datamapping"
)

type Mapper interface {
	Map(map[string]string) (document.Article, error)
}

type MappingLoader interface {
	Load(validate bool) (*datamapping.DataMapper, error)
}
