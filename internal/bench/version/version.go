// Package version exposes the bench tool identity used in artifact provenance.
package version

import "fmt"

// Semver is the semantic version. Bump on every release that changes artifact schemas or CLI.
const Semver = "1.0.0"

// Tool is the identifier every artifact's meta block records.
func Tool() string {
	return "bench/" + Semver
}

// SchemaVersion is the current on-disk schema version for all bench artifacts.
// Bumped whenever any of (spec.yaml, suite.yaml, pool.yaml, annotations.yaml,
// report.json) changes shape. Loaders reject anything else.
const SchemaVersion = 1

// CheckSchema returns an error if got != SchemaVersion. Use in every loader.
func CheckSchema(got int, artifact string) error {
	if got == SchemaVersion {
		return nil
	}
	if got == 0 {
		return fmt.Errorf("%s: missing schema_version (expected %d) — regenerate with bench %s",
			artifact, SchemaVersion, regenHint(artifact))
	}
	return fmt.Errorf("%s: schema_version %d not supported (expected %d) — regenerate with bench %s",
		artifact, got, SchemaVersion, regenHint(artifact))
}

func regenHint(artifact string) string {
	switch artifact {
	case "spec", "suite":
		return "init"
	case "pool":
		return "pool"
	case "annotations":
		return "judge"
	case "report":
		return "run"
	default:
		return "init"
	}
}
