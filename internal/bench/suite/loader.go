package suite

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/tadjov/tusker/internal/bench/version"
	"gopkg.in/yaml.v3"
)

type LoadedSuite struct {
	Suite    *TestSuite
	Registry *TemplateRegistry
	Dir      string
}

func LoadFromFile(path string) (*LoadedSuite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read suite file: %w", err)
	}
	loaded, err := Parse(data)
	if err != nil {
		return nil, err
	}
	loaded.Dir = filepath.Dir(path)
	return loaded, nil
}

func Parse(data []byte) (*LoadedSuite, error) {
	var s TestSuite
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse suite YAML: %w", err)
	}
	if err := version.CheckSchema(s.SchemaVersion, "suite"); err != nil {
		return nil, err
	}
	if s.ID == "" {
		return nil, fmt.Errorf("suite is missing required field: id")
	}
	if len(s.Queries) == 0 {
		return nil, fmt.Errorf("suite has no queries")
	}

	registry := NewTemplateRegistry()
	for _, t := range s.Templates {
		if err := registry.Register(t); err != nil {
			return nil, fmt.Errorf("register template: %w", err)
		}
	}

	for i, q := range s.Queries {
		if q.ID == "" {
			return nil, fmt.Errorf("query at index %d has no id", i)
		}
		if len(q.Engines) == 0 {
			return nil, fmt.Errorf("query %q has no engines", q.ID)
		}
		if q.Category != "" {
			if _, err := ParseCategory(string(q.Category)); err != nil {
				return nil, fmt.Errorf("query %q: %w", q.ID, err)
			}
		}
		if err := checkGatedText(q); err != nil {
			return nil, fmt.Errorf("query %q: %w", q.ID, err)
		}
		if err := q.Filters.check(); err != nil {
			return nil, fmt.Errorf("query %q: %w", q.ID, err)
		}
		for engName, eq := range q.Engines {
			if eq.Template != "" {
				if _, ok := registry.Get(eq.Template); !ok {
					return nil, fmt.Errorf("query %q engine %q references unknown template %q", q.ID, engName, eq.Template)
				}
			}
			if err := checkEngineQuery(&eq, registry); err != nil {
				return nil, fmt.Errorf("query %q engine %q: %w", q.ID, engName, err)
			}
		}
	}

	return &LoadedSuite{Suite: &s, Registry: registry}, nil
}

// dottedForm is a run of letters joined by dots, such as U.S. or e.g.
var dottedForm = regexp.MustCompile(`[\p{L}\p{N}]*\p{L}(?:\.\p{L}[\p{L}\p{N}]*)+\.?`)

// checkGatedText refuses the text of a phrase or boolean query that its gate
// would read otherwise than the engines: a boolean text that does not parse,
// or a dotted form, which Postgres and Elasticsearch keep as one token and the
// judges split at each dot.
func checkGatedText(q Query) error {
	if q.Category != CategoryPhrase && q.Category != CategoryBoolean {
		return nil
	}
	if dotted := dottedForm.FindString(q.Text); dotted != "" {
		return fmt.Errorf("text %q holds the dotted form %q, which the engines read as one token and the judges as several: write it without dots", q.Text, dotted)
	}
	if q.Category == CategoryBoolean && q.Text != "" {
		if _, err := ParseBoolean(q.Text); err != nil {
			return err
		}
	}
	return nil
}

func checkEngineQuery(eq *EngineQuery, registry *TemplateRegistry) error {
	if _, ok := eq.Params[QueryVectorArg]; ok {
		return fmt.Errorf("param %q is reserved for the query vector the run supplies", QueryVectorArg)
	}
	args := eq.declaredArgs(registry)
	for _, name := range slices.Sorted(maps.Keys(eq.Params)) {
		if !slices.Contains(args, name) {
			return fmt.Errorf("param %q is not one of the args %v", name, args)
		}
	}
	return nil
}

// Block is one engine's statement for one query, before the run adds the query vector.
type Block struct {
	QueryID   string
	Statement string
	Args      []string
	// Template is empty for an inline or file query.
	Template string
	Params   TemplateParams
}

// Blocks reads engine's statement in every query, with defaults under the query's own params.
func (ls *LoadedSuite) Blocks(engine string, defaults TemplateParams) ([]Block, error) {
	var blocks []Block
	for _, q := range ls.Suite.Queries {
		eq, ok := q.Engines[engine]
		if !ok {
			continue
		}
		statement, args, err := eq.statement(ls.Registry, ls.Dir)
		if err != nil {
			return nil, fmt.Errorf("query %q engine %q: %w", q.ID, engine, err)
		}
		blocks = append(blocks, Block{
			QueryID:   q.ID,
			Statement: statement,
			Args:      args,
			Template:  eq.Template,
			Params:    mergeParams(defaults, eq.Params),
		})
	}
	return blocks, nil
}

// CheckArgsSupplied verifies that every arg of every block engine reads is
// supplied, by the query's params, the engine's defaults or the run's query
// vector.
func (ls *LoadedSuite) CheckArgsSupplied(engine string, defaults TemplateParams) error {
	for _, q := range ls.Suite.Queries {
		eq, ok := q.Engines[engine]
		if !ok {
			continue
		}
		var missing []string
		for _, name := range eq.declaredArgs(ls.Registry) {
			_, inQuery := eq.Params[name]
			_, inDefaults := defaults[name]
			if !inQuery && !inDefaults && name != QueryVectorArg {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("query %q engine %q: no param supplies args %v", q.ID, engine, missing)
		}
	}
	return nil
}

// NeedsQueryVector reports whether any block of q takes the query vector.
func (ls *LoadedSuite) NeedsQueryVector(q *Query) bool {
	for _, eq := range q.Engines {
		if slices.Contains(eq.declaredArgs(ls.Registry), QueryVectorArg) {
			return true
		}
	}
	return false
}
