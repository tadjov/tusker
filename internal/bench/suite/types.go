package suite

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// QueryVectorArg is the reserved arg the runner fills with the query's
// embedding, so a statement needs the vector exactly when its args name it.
// Postgres receives it as pgvector text, cast in SQL with $N::vector.
const QueryVectorArg = "query_vector"

// EmbeddingModelParam names the engine-level param carrying the embedding model
// an arm is measured against. article_embeddings is keyed (article_id,
// model_name), so a vector template filters on it and the runner refuses to run
// an engine whose declared value disagrees with the model embedding the query.
const EmbeddingModelParam = "embedding_model"

type TestSuite struct {
	SchemaVersion int              `yaml:"schema_version"`
	ID            string           `yaml:"id"`
	Name          string           `yaml:"name,omitempty"`
	Description   string           `yaml:"description,omitempty"`
	Version       string           `yaml:"version"`
	Corpus        *Corpus          `yaml:"corpus,omitempty"`
	Templates     []*QueryTemplate `yaml:"templates,omitempty"`
	Queries       []Query          `yaml:"queries"`
}

// Corpus records the dataset the suite targets. Lets a report attest "this
// was scored against the news_hunter_articles index, snapshot 2026-05-10".
type Corpus struct {
	Name       string `yaml:"name"`
	Source     string `yaml:"source,omitempty"`
	SnapshotAt string `yaml:"snapshot_at,omitempty"`
}

type Query struct {
	ID          string `yaml:"id"`
	Description string `yaml:"description"`
	// Category drives per-category aggregation and the judge's grading rule.
	Category  string                 `yaml:"category,omitempty"`
	Engines   map[string]EngineQuery `yaml:"engines"`
	Judgments []RelevanceJudgment    `yaml:"judgments"`
}

type EngineQuery struct {
	Query    string `yaml:"query,omitempty"`
	File     string `yaml:"file,omitempty"`
	Template string `yaml:"template,omitempty"`
	// Args name the param behind each $N of an inline or file query, in order.
	Args   []string       `yaml:"args,omitempty"`
	Params TemplateParams `yaml:"params,omitempty"`
}

func (eq *EngineQuery) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		eq.Query = value.Value
		return nil
	}
	if err := checkEngineQueryKeys(value); err != nil {
		return err
	}
	type plain EngineQuery
	return value.Decode((*plain)(eq))
}

// checkEngineQueryKeys exists because Node.Decode does not inherit the
// decoder's KnownFields.
func checkEngineQueryKeys(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return nil
	}
	known := []string{"query", "file", "template", "args", "params"}
	for i := 0; i < len(value.Content); i += 2 {
		key := value.Content[i]
		if !slices.Contains(known, key.Value) {
			return fmt.Errorf("line %d: field %s not found in an engine query", key.Line, key.Value)
		}
	}
	return nil
}

// ResolveOptions addresses one engine's query inside a suite query and carries
// what the engine and the run add to the query's own params.
type ResolveOptions struct {
	// Engine names the per-query block to read. Under engine aliasing this is
	// the engine that owns the queries, which need not be the one being
	// measured.
	Engine   string
	Registry *TemplateRegistry
	SuiteDir string
	// Defaults are the running engine's declared params; the query's own params
	// win over them.
	Defaults TemplateParams
	// QueryVector fills the reserved QueryVectorArg.
	QueryVector []float32
}

// Resolve reads the engine's statement and looks up each of its args in the
// query's params, the engine defaults and the query vector.
func (eq *EngineQuery) Resolve(opts ResolveOptions) (*ResolvedQuery, error) {
	params := mergeParams(opts.Defaults, eq.Params)
	if opts.QueryVector != nil {
		params[QueryVectorArg] = opts.QueryVector
	}
	statement, argNames, err := eq.statement(opts.Registry, opts.SuiteDir)
	if err != nil {
		return nil, err
	}
	args, err := valuesInOrder(argNames, params)
	if err != nil {
		return nil, err
	}
	return &ResolvedQuery{Query: statement, Args: args, Template: eq.Template, Params: params}, nil
}

func (eq *EngineQuery) statement(registry *TemplateRegistry, suiteDir string) (string, []string, error) {
	switch {
	case eq.Template != "":
		if registry == nil {
			return "", nil, fmt.Errorf("template %q referenced but no registry available", eq.Template)
		}
		t, ok := registry.Get(eq.Template)
		if !ok {
			return "", nil, fmt.Errorf("template %q not found", eq.Template)
		}
		return t.Query, t.Args, nil
	case eq.File != "":
		path := eq.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(suiteDir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("read query file %q: %w", eq.File, err)
		}
		return string(data), eq.Args, nil
	default:
		return eq.Query, eq.Args, nil
	}
}

// declaredArgs are the args of the statement the block runs. The template must
// already be known to the registry.
func (eq *EngineQuery) declaredArgs(registry *TemplateRegistry) []string {
	if eq.Template != "" {
		if t, ok := registry.Get(eq.Template); ok {
			return t.Args
		}
	}
	return eq.Args
}

// mergeParams overlays layers left to right into a fresh map, so a narrower
// layer wins and no caller's map is aliased — engine params are owned by the
// spec and shared by every query the engine runs.
func mergeParams(layers ...TemplateParams) TemplateParams {
	out := TemplateParams{}
	for _, layer := range layers {
		for k, v := range layer {
			out[k] = v
		}
	}
	return out
}

type ResolvedQuery struct {
	Query string
	// Args are the param values the statement's args name, in order.
	Args []any
	// Template is the suite template the query came from, empty for an inline
	// or file query.
	Template string
	// Params are every value the query, its engine and the run supply, with the
	// query vector as numbers, for an engine that renders the template itself.
	Params TemplateParams
}

type RelevanceJudgment struct {
	DocID     uuid.UUID `yaml:"doc_id"`
	Relevance int       `yaml:"relevance"`
}

func (q *Query) JudgmentMap() map[uuid.UUID]int {
	m := make(map[uuid.UUID]int, len(q.Judgments))
	for _, j := range q.Judgments {
		m[j.DocID] = j.Relevance
	}
	return m
}

// InjectJudgments sets the per-query Judgments slice from a flat map produced
// by the CLI layer after loading an annotations file. Replaces the loader-side
// auto-injection of v0; keeps the suite YAML focused on queries only.
func (ls *LoadedSuite) InjectJudgments(byQuery map[string][]RelevanceJudgment) {
	for i := range ls.Suite.Queries {
		if js, ok := byQuery[ls.Suite.Queries[i].ID]; ok {
			ls.Suite.Queries[i].Judgments = js
		}
	}
}

// ResolveEngineQuery renders the block named by opts.Engine, or returns a nil
// query when the suite declares none for it.
func (q *Query) ResolveEngineQuery(opts ResolveOptions) (*ResolvedQuery, error) {
	eq, ok := q.Engines[opts.Engine]
	if !ok {
		return nil, nil
	}
	resolved, err := eq.Resolve(opts)
	if err != nil {
		return nil, fmt.Errorf("query %q engine %q: %w", q.ID, opts.Engine, err)
	}
	return resolved, nil
}

// FormatVector renders a float vector as pgvector's text input.
func FormatVector(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
