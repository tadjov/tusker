package spec

import "time"

type BenchSpec struct {
	SchemaVersion int               `yaml:"schema_version"`
	ID            string            `yaml:"id"`
	Kind          Kind              `yaml:"kind,omitempty"`
	Description   string            `yaml:"description,omitempty"`
	Defaults      Defaults          `yaml:"defaults,omitempty"`
	Engines       map[string]Engine `yaml:"engines"`
	Metrics       MetricsConfig     `yaml:"metrics"`
	Runs          RunsConfig        `yaml:"runs"`
	Jobs          []Job             `yaml:"jobs"`

	// Warnings collects non-fatal load-time advisories (e.g. kind omitted). It is
	// populated by the loader and never serialized; the CLI prints it once.
	Warnings []string `yaml:"-"`
}

// Kind names the IR paradigm a track measures. It is primarily a taxonomy /
// provenance label (one per track, aligned with the thesis's search
// paradigms); requirements such as "needs an embedder" are derived from it
// rather than declared separately. Optional — an empty kind is valid but the
// loader warns, since it disables the derived preconditions.
type Kind string

const (
	KindFTS        Kind = "fts"
	KindStructured Kind = "structured"
	KindFuzzy      Kind = "fuzzy"
	KindSemantic   Kind = "semantic"
	KindHybrid     Kind = "hybrid"
)

// Valid reports whether k is empty (allowed — kind is optional) or one of the
// known paradigms. An unknown non-empty value is a hard error at load.
func (k Kind) Valid() bool {
	switch k {
	case "", KindFTS, KindStructured, KindFuzzy, KindSemantic, KindHybrid:
		return true
	default:
		return false
	}
}

// RequiresEmbedder reports whether the paradigm needs a live query embedder
// (EMBEDDING_BASE_URL + an embedding-capable engine). Semantic and hybrid
// queries take the reserved query_vector arg; the rest are lexical and resolve
// without one.
func (k Kind) RequiresEmbedder() bool {
	return k == KindSemantic || k == KindHybrid
}

// Defaults supply fallback values that the CLI flags can override. Lets users
// set "this track defaults to keyword judgments and pool depth 100" in one
// place instead of repeating flags.
type Defaults struct {
	PoolDepth int    `yaml:"pool_depth,omitempty"`
	Judgments string `yaml:"judgments,omitempty"` // strategy name OR path
}

type Job struct {
	Name    string   `yaml:"name"`
	Suite   string   `yaml:"suite"`
	Engines []string `yaml:"engines"`
}

// EngineType is the kind of system an engine runs queries against.
type EngineType string

const (
	EnginePostgres      EngineType = "postgres"
	EngineElasticsearch EngineType = "elasticsearch"
	EngineAPI           EngineType = "api"
)

func (t EngineType) Valid() bool {
	switch t {
	case EnginePostgres, EngineElasticsearch, EngineAPI:
		return true
	default:
		return false
	}
}

type Engine struct {
	Type       EngineType `yaml:"type"`
	Connection string     `yaml:"connection"`
	Index      string     `yaml:"index,omitempty"`
	// ConnectionSettings are GUCs pinning the operating point a postgres engine
	// is measured at, e.g. hnsw.ef_search. See docs/bench.md.
	ConnectionSettings map[string]string `yaml:"connection_settings,omitempty"`
	// Params are template defaults for every query this engine runs, overridden
	// by the query's own params. Two engines differing only in a ranking
	// argument declare it here instead of in every query block.
	//
	// Typed as map[string]any rather than suite.TemplateParams so spec keeps no
	// dependency on suite; the two are identical and convert at the call site.
	Params map[string]any `yaml:"params,omitempty"`
	// QueriesFrom names the engine whose per-query engines: block this engine
	// reuses, so an A/B arm cannot drift from the arm it is compared against.
	QueriesFrom string `yaml:"queries_from,omitempty"`
}

// QueryBinding is how one engine reaches its queries: the engine whose
// per-query block supplies the query text, and the params that engine
// contributes as template defaults.
type QueryBinding struct {
	QuerySource string
	Params      map[string]any
}

// QueryBinding resolves engine aliasing for the named engine. Aliasing is one
// level deep — validate rejects a chain — so this never walks a graph. An
// undeclared engine binds to itself, leaving the "unknown engine" error to the
// caller that owns it.
func (s *BenchSpec) QueryBinding(name string) QueryBinding {
	eng, ok := s.Engines[name]
	if !ok {
		return QueryBinding{QuerySource: name}
	}
	if eng.QueriesFrom != "" {
		return QueryBinding{QuerySource: eng.QueriesFrom, Params: eng.Params}
	}
	return QueryBinding{QuerySource: name, Params: eng.Params}
}

type MetricsConfig struct {
	KValues            []int `yaml:"k_values"`
	RelevanceThreshold int   `yaml:"relevance_threshold"`
}

// DefaultQueryTimeout bounds one execution of one query when the spec sets no
// query_timeout.
const DefaultQueryTimeout = 30 * time.Second

type RunsConfig struct {
	Warmup     int `yaml:"warmup"`
	Iterations int `yaml:"iterations"`
	// QueryTimeout is the deadline of every single execution, warmup or
	// measured, on every engine type.
	QueryTimeout time.Duration `yaml:"query_timeout"`
}
