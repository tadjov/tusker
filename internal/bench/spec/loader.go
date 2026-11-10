package spec

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/DjordjeVuckovic/tusker/internal/bench/version"
	"gopkg.in/yaml.v3"
)

// expandConnectionEnvVars substitutes ${VAR} / $VAR references in every
// engine's connection string. This keeps secrets out of spec.yaml — authors
// write connection: "${PG_DSN}" and the value is pulled from the environment
// at load time, so it never appears in committed YAML or in report provenance.
func expandConnectionEnvVars(bs *BenchSpec) {
	for name, eng := range bs.Engines {
		eng.Connection = os.ExpandEnv(eng.Connection)
		bs.Engines[name] = eng
	}
}

// LoadFromFile reads, parses, and validates a spec YAML. Relative paths in
// jobs[].suite are rewritten to be absolute and rooted at the spec file's
// directory — so downstream loaders work regardless of process CWD.
//
// This is the only place that does the path rewrite; runner/cmd_pool/
// cmd_validate consume the resolved spec and never need to know where the
// spec was loaded from.
func LoadFromFile(path string) (*BenchSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read spec file: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("abs spec path: %w", err)
	}
	bs, err := Parse(data)
	if err != nil {
		return nil, err
	}
	resolveJobSuitePaths(bs, filepath.Dir(abs))
	expandConnectionEnvVars(bs)
	return bs, nil
}

// resolveJobSuitePaths rewrites every job's relative Suite path to absolute,
// anchored at the spec file's directory. Absolute paths are left alone.
func resolveJobSuitePaths(bs *BenchSpec, specDir string) {
	for i := range bs.Jobs {
		s := bs.Jobs[i].Suite
		if s == "" || filepath.IsAbs(s) {
			continue
		}
		bs.Jobs[i].Suite = filepath.Join(specDir, s)
	}
}

func Parse(data []byte) (*BenchSpec, error) {
	var s BenchSpec
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse spec YAML: %w", err)
	}
	if err := validate(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func validate(s *BenchSpec) error {
	if err := version.CheckSchema(s.SchemaVersion, "spec"); err != nil {
		return err
	}
	if s.ID == "" {
		return fmt.Errorf("spec is missing required field: id")
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("spec has invalid kind %q (want one of: fts, structured, fuzzy, semantic, hybrid)", s.Kind)
	}
	if s.Kind == "" {
		s.Warnings = append(s.Warnings,
			"spec has no kind — set one of fts|structured|fuzzy|semantic|hybrid to enable paradigm preconditions")
	}
	if len(s.Jobs) == 0 {
		return fmt.Errorf("spec has no jobs")
	}
	if len(s.Engines) == 0 {
		return fmt.Errorf("spec has no engines")
	}
	for i, j := range s.Jobs {
		if j.Name == "" {
			return fmt.Errorf("job at index %d has no name", i)
		}
		if j.Suite == "" {
			return fmt.Errorf("job %q has no suite", j.Name)
		}
		if len(j.Engines) == 0 {
			return fmt.Errorf("job %q has no engines", j.Name)
		}
		for _, engRef := range j.Engines {
			if _, ok := s.Engines[engRef]; !ok {
				return fmt.Errorf("job %q references unknown engine %q", j.Name, engRef)
			}
		}
	}
	for name, eng := range s.Engines {
		if eng.Type == "" {
			return fmt.Errorf("engine %q has no type", name)
		}
		if !eng.Type.Valid() {
			return fmt.Errorf("engine %q has invalid type %q", name, eng.Type)
		}
		if eng.Connection == "" {
			return fmt.Errorf("engine %q has no connection", name)
		}
		if err := validateQueriesFrom(s, name, eng); err != nil {
			return err
		}
	}
	if err := validateDefaults(s); err != nil {
		return err
	}
	if len(s.Metrics.KValues) == 0 {
		s.Metrics.KValues = []int{3, 5, 10}
	}
	if err := CheckKValues(s.Metrics.KValues, s.Defaults.PoolDepth); err != nil {
		return fmt.Errorf("metrics.k_values: %w", err)
	}
	if s.Metrics.RelevanceThreshold <= 0 {
		s.Metrics.RelevanceThreshold = 1
	}
	if s.Runs.Warmup <= 0 {
		s.Runs.Warmup = 1
	}
	if s.Runs.Iterations <= 0 {
		s.Runs.Iterations = 3
	}
	if s.Runs.QueryTimeout < 0 {
		return fmt.Errorf("runs.query_timeout %s is negative", s.Runs.QueryTimeout)
	}
	if s.Runs.QueryTimeout == 0 {
		s.Runs.QueryTimeout = DefaultQueryTimeout
	}
	return nil
}

// validateQueriesFrom checks an engine alias points at a declared engine of its
// own type other than itself, and that the target is not an alias in turn.
// Aliasing is deliberately one level: resolution never walks a chain, so a
// chain declared here would silently resolve to the wrong query block.
func validateQueriesFrom(s *BenchSpec, name string, eng Engine) error {
	if eng.QueriesFrom == "" {
		return nil
	}
	if eng.QueriesFrom == name {
		return fmt.Errorf("engine %q has queries_from pointing at itself", name)
	}
	target, ok := s.Engines[eng.QueriesFrom]
	if !ok {
		return fmt.Errorf("engine %q has queries_from %q, which is not a declared engine", name, eng.QueriesFrom)
	}
	if target.QueriesFrom != "" {
		return fmt.Errorf("engine %q has queries_from %q, which is itself an alias of %q — queries_from does not chain",
			name, eng.QueriesFrom, target.QueriesFrom)
	}
	if target.Type != eng.Type {
		return fmt.Errorf("engine %q (type %s) has queries_from %q (type %s): an alias must reuse a block written for its own engine type",
			name, eng.Type, eng.QueriesFrom, target.Type)
	}
	return nil
}

// KnownStrategies is set at startup by the cmd layer (which has the registry).
// Keeping spec free of a hard dep on judgment avoids an upstream import cycle
// risk if judgment ever needs spec types. Nil means "skip validation".
var KnownStrategies func() []string

// validateDefaults checks defaults.judgments shape:
//   - empty → ok (CLI must supply --judgments)
//   - path-like (contains "/" or ends in .yaml/.yml) → trusted, validated at load
//   - bare name → must be a known strategy
func validateDefaults(s *BenchSpec) error {
	v := s.Defaults.Judgments
	if v == "" {
		return nil
	}
	if looksLikeJudgmentsPath(v) {
		return nil
	}
	if KnownStrategies == nil {
		return nil // CLI didn't wire the registry; defer to runtime
	}
	for _, k := range KnownStrategies() {
		if k == v {
			return nil
		}
	}
	return fmt.Errorf("spec.defaults.judgments=%q is not a known strategy and not a path (expected one of: %v, or a path to an annotations YAML)",
		v, KnownStrategies())
}

func looksLikeJudgmentsPath(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r == '/' || r == filepath.Separator {
			return true
		}
	}
	ext := filepath.Ext(s)
	return ext == ".yaml" || ext == ".yml"
}
