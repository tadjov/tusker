package dialect

import (
	"fmt"

	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/suite"
)

// Literal is the tusker API: a request descriptor sent as written.
type Literal struct{}

func (Literal) Name() string { return "literal" }

func (Literal) StoresTemplates() bool { return false }

func (Literal) Check(block suite.Block) error {
	if len(block.Args) > 0 {
		return fmt.Errorf("args %v: a literal request takes no params, write the values into it", block.Args)
	}
	if names := readMustache(block.Statement).all; len(names) > 0 {
		return fmt.Errorf("reads %v, which nothing renders for this engine", names)
	}
	return nil
}

func (Literal) Request(_ string, query *suite.ResolvedQuery) engine.Request {
	return engine.Request{Query: query.Query}
}
