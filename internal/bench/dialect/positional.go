package dialect

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"

	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/suite"
)

// Positional is Postgres: a statement with $1 … $n, bound in args order.
type Positional struct{}

var placeholder = regexp.MustCompile(`\$(\d+)`)

func (Positional) Name() string { return "positional" }

func (Positional) StoresTemplates() bool { return false }

func (Positional) Check(block suite.Block) error {
	used := map[int]bool{}
	for _, m := range placeholder.FindAllStringSubmatch(block.Statement, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return fmt.Errorf("placeholder $%s: %w", m[1], err)
		}
		used[n] = true
	}
	highest := 0
	if len(used) > 0 {
		highest = slices.Max(slices.Collect(maps.Keys(used)))
	}
	if highest > len(block.Args) {
		return fmt.Errorf("statement uses $%d but args names only %d: %v", highest, len(block.Args), block.Args)
	}
	if highest < len(block.Args) {
		return fmt.Errorf("args %v name %d values but the statement takes $1…$%d", block.Args, len(block.Args), highest)
	}
	for n := 1; n <= highest; n++ {
		if !used[n] {
			return fmt.Errorf("statement skips $%d", n)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(block.Params)) {
		switch block.Params[name].(type) {
		case []any, map[string]any:
			return fmt.Errorf("param %q is a list or map, but a placeholder binds one value", name)
		}
	}
	return nil
}

func (Positional) Request(_ string, query *suite.ResolvedQuery) engine.Request {
	args := make([]any, len(query.Args))
	for i, value := range query.Args {
		args[i] = bindValue(value)
	}
	return engine.Request{Query: query.Query, Args: args}
}

// bindValue sends text for the statement to cast, and nil so null binds as SQL NULL.
func bindValue(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case []float32:
		return suite.FormatVector(v)
	default:
		return fmt.Sprint(v)
	}
}
