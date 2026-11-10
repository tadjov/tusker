// Package version exposes the bench tool identity used in artifact provenance.
package version

import (
	"fmt"
	"runtime/debug"
	"sync"
)

// Semver is the semantic version. Bump on every release that changes artifact schemas or CLI.
const Semver = "1.0.0"

// Tool identifies the build in every artifact's meta block: "bench/1.0.0",
// plus "+<12-char revision>" and "-modified" when the binary was built from a
// VCS checkout. go run and test binaries carry no revision.
func Tool() string {
	toolOnce.Do(func() {
		info, _ := debug.ReadBuildInfo()
		toolStr = toolOf(info)
	})
	return toolStr
}

func toolOf(info *debug.BuildInfo) string {
	tool := "bench/" + Semver
	if info == nil {
		return tool
	}
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return tool
	}
	tool += "+" + revision[:min(12, len(revision))]
	if modified {
		tool += "-modified"
	}
	return tool
}

var (
	toolOnce sync.Once
	toolStr  string
)

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
