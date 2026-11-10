// Package trackctx resolves the "track" — the self-contained folder that
// owns a benchmark's spec, suite, pool, annotations, and reports. Every
// subcommand goes through Resolve (single track) or ResolveGlob (a group) to
// figure out which paths it operates on.
//
// A track is any folder holding spec.yaml + suite.yaml + trec/. Nothing above
// that folder is enforced or assumed: tracks/global-news-dataset/fts_quality
// and benches/b1 are equally valid — only the layout inside a track is a
// convention.
//
// Track args are ordinary filesystem paths:
//   - absolute            → used as-is
//   - relative            → resolved against the track root
//   - glob (* ? [)        → expanded by ResolveGlob across track-shaped matches
//   - empty               → walk up from cwd to the nearest track-shaped folder
//
// The track root (Inputs.TrackRoot, from --track-root / BENCH_TRACK_ROOT)
// defaults to the current directory, so paths behave the way they do in any
// other tool. Pointing it at the folder that holds your tracks is pure
// convenience: with BENCH_TRACK_ROOT=tracks/global-news-dataset,
// `bench run fts_quality` is the same as
// `bench run tracks/global-news-dataset/fts_quality` from the repo root, or as
// cd-ing into the track and passing no arg at all.
//
// Grouping is explicit: only a glob fans out. A single name always means
// exactly one track — a directory of tracks never implicitly becomes a group.
package trackctx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	specFile     = "spec.yaml"
	suiteFile    = "suite.yaml"
	trecDir      = "trec"
	poolFile     = "pool.yaml"
	reportsDir   = "reports"
	latestReport = "latest.json"
)

// Inputs lets callers pass explicit overrides that beat track inference.
// Any field left blank falls back to the track convention.
type Inputs struct {
	TrackArg   string // --track value OR positional arg (already merged by caller)
	TrackRoot  string // --track-root / BENCH_TRACK_ROOT; base for relative args (default: cwd)
	SpecPath   string // --spec override
	PoolPath   string // --pool override
	OutputPath string // --output override
	Judgments  string // --judgments value: "lexical" (strategy name) | path/to.yaml | empty
}

// Track holds absolute paths derived from a track folder + overrides.
type Track struct {
	Root string // absolute path to the track folder
	Spec string
	Pool string

	name       string // root-relative id, e.g. "fts_quality" or "tracks/news/fts"
	trecDir    string
	reportsDir string
}

// Resolve resolves a single track and layers any explicit overrides on top.
func Resolve(in Inputs) (*Track, error) {
	root, name, err := resolveRoot(in.TrackArg, in.TrackRoot)
	if err != nil {
		return nil, err
	}
	t := newTrack(root, name)
	t.Spec = firstNonEmpty(in.SpecPath, t.Spec)
	t.Pool = firstNonEmpty(in.PoolPath, t.Pool)
	return t, nil
}

// ResolveGlob expands a glob pattern to every track-shaped match under the
// track root, sorted, naming each by its path relative to that root.
func ResolveGlob(pattern, trackRoot string) ([]*Track, error) {
	base := rootDir(trackRoot)
	matches, err := filepath.Glob(underRoot(base, pattern))
	if err != nil {
		return nil, fmt.Errorf("bad track pattern %q: %w", pattern, err)
	}
	sort.Strings(matches)
	tracks := make([]*Track, 0, len(matches))
	for _, m := range matches {
		abs, err := filepath.Abs(m)
		if err != nil || !isTrackShaped(abs) {
			continue
		}
		abs = canonicalise(abs)
		tracks = append(tracks, newTrack(abs, relName(base, abs)))
	}
	if len(tracks) == 0 {
		return nil, fmt.Errorf("no tracks match pattern %q under %s", pattern, rootLabel(base))
	}
	return tracks, nil
}

// IsPattern reports whether arg is a glob (news/*) rather than a single path.
func IsPattern(arg string) bool {
	return strings.ContainsAny(arg, "*?[")
}

// newTrack builds a Track with all paths derived from the convention.
func newTrack(root, name string) *Track {
	t := &Track{
		Root:       root,
		name:       name,
		trecDir:    filepath.Join(root, trecDir),
		reportsDir: filepath.Join(root, reportsDir),
	}
	t.Spec = filepath.Join(root, specFile)
	t.Pool = filepath.Join(t.trecDir, poolFile)
	return t
}

// JudgmentsPath resolves --judgments. Three shapes:
//   - "" or absent → track's default (caller should consult spec.defaults).
//   - bare name like "lexical" → trec/annotations.lexical.yaml.
//   - path containing / or .yaml → used verbatim.
func (t *Track) JudgmentsPath(value string) string {
	if value == "" {
		return ""
	}
	if isPath(value) {
		return value
	}
	return filepath.Join(t.trecDir, "annotations."+value+".yaml")
}

// DefaultJudgmentsPath resolves spec.defaults.judgments like JudgmentsPath,
// except a relative path starts at the spec file's folder rather than the cwd,
// the same anchor the spec's suite paths use.
func (t *Track) DefaultJudgmentsPath(value string) string {
	path := t.JudgmentsPath(value)
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	specDir, err := filepath.Abs(filepath.Dir(t.Spec))
	if err != nil {
		specDir = filepath.Dir(t.Spec)
	}
	return filepath.Join(specDir, path)
}

// QrelsPath mirrors JudgmentsPath for TREC qrels exports. An annotations YAML
// path maps to the qrels.<name>.tsv beside it, so an export given the file it
// reads never writes over it; any other path is used verbatim.
func (t *Track) QrelsPath(value string) string {
	if value == "" {
		return ""
	}
	if !isPath(value) {
		return filepath.Join(t.trecDir, "qrels."+value+".tsv")
	}
	ext := filepath.Ext(value)
	if ext != ".yaml" && ext != ".yml" {
		return value
	}
	name := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(value), ext), "annotations.")
	return filepath.Join(filepath.Dir(value), "qrels."+name+".tsv")
}

// ReportPath returns <track>/reports/<run_id>.json. Callers pass the run_id
// from meta.NewRunID("run").
func (t *Track) ReportPath(runID string) string {
	return filepath.Join(t.reportsDir, runID+".json")
}

// LatestReportPath is the conventional pointer to the most-recent report.
func (t *Track) LatestReportPath() string {
	return filepath.Join(t.reportsDir, latestReport)
}

// Name returns the track's id relative to the track root — the same string you
// would type to address it. Used for display and progress banners.
func (t *Track) Name() string {
	if t.name != "" {
		return t.name
	}
	return filepath.Base(t.Root)
}

// resolveRoot turns a track arg into an absolute track folder plus the
// root-relative name that addresses it.
func resolveRoot(arg, trackRoot string) (root, name string, err error) {
	base := rootDir(trackRoot)
	if arg == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", "", fmt.Errorf("getwd: %w", err)
		}
		found := walkUp(cwd)
		if found == "" {
			return "", "", errors.New(
				"no track specified and current directory is not inside a track folder.\n" +
					"  Pass a track path:  bench <cmd> <track>\n" +
					"  Or scaffold one:    bench init <name>")
		}
		found = canonicalise(found)
		return found, relName(base, found), nil
	}

	abs, err := filepath.Abs(underRoot(base, arg))
	if err != nil {
		return "", "", fmt.Errorf("abs %q: %w", arg, err)
	}
	if !isTrackShaped(abs) {
		return "", "", notFoundError(arg, abs, base)
	}
	abs = canonicalise(abs)
	return abs, relName(base, abs), nil
}

// rootDir normalises the configured track root; empty means "current directory".
func rootDir(trackRoot string) string {
	if trackRoot == "" {
		return "."
	}
	return trackRoot
}

// underRoot resolves a track arg against the root. Absolute args ignore it,
// exactly as a shell would.
func underRoot(base, arg string) string {
	if filepath.IsAbs(arg) {
		return arg
	}
	return filepath.Join(base, arg)
}

// rootLabel renders the root for error messages.
func rootLabel(base string) string {
	if base == "." {
		return "the current directory"
	}
	return base + "/"
}

// relName names a track by its path relative to the root, falling back to the
// base name for tracks that live outside it.
func relName(base, abs string) string {
	baseAbs, err := filepath.Abs(base)
	if err == nil {
		// rel == "." means the root IS the track (you cd'd into it) — name it
		// after the folder rather than the useless ".".
		if rel, rerr := filepath.Rel(canonicalise(baseAbs), abs); rerr == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(abs)
}

// notFoundError reports the path that was tried and, when a folder of that name
// sits a little deeper, names it — the usual symptom of an unset or too-shallow
// track root. Suggestions are advisory; resolution never wanders on its own.
func notFoundError(arg, tried, base string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "track %q not found\n  tried: %s", arg, tried)
	if found := suggestTracks(base, arg); len(found) > 0 {
		fmt.Fprintf(&b, "\n  did you mean: %s", strings.Join(found, ", "))
	}
	fmt.Fprintf(&b, "\n  hint: --track-root / BENCH_TRACK_ROOT sets where relative track paths start")
	fmt.Fprintf(&b, "\n  scaffold it: bench init %s", arg)
	return errors.New(b.String())
}

// suggestTracks looks up to three levels below the root for a track folder
// whose name matches the arg. The nearest depth with a hit wins.
func suggestTracks(base, arg string) []string {
	name := filepath.Base(filepath.Clean(arg))
	if name == "" || name == "." || name == string(filepath.Separator) || IsPattern(name) {
		return nil
	}
	for _, depth := range []string{"*", "*/*", "*/*/*"} {
		matches, err := filepath.Glob(filepath.Join(base, depth, name))
		if err != nil {
			return nil
		}
		sort.Strings(matches)
		found := make([]string, 0, len(matches))
		for _, m := range matches {
			if abs, aerr := filepath.Abs(m); aerr == nil && isTrackShaped(abs) {
				found = append(found, filepath.ToSlash(filepath.Clean(m)))
			}
		}
		if len(found) > 0 {
			return found
		}
	}
	return nil
}

// canonicalise resolves symlinks so a track's Root is stable across OS quirks
// (notably macOS where /tmp -> /private/tmp). Failure falls back to the input.
func canonicalise(p string) string {
	if realPath, err := filepath.EvalSymlinks(p); err == nil {
		return realPath
	}
	return p
}

// walkUp searches cwd and its parents for a track-shaped folder. Stops at the
// filesystem root or after 16 hops, whichever first.
func walkUp(start string) string {
	dir := start
	for i := 0; i < 16; i++ {
		if isTrackShaped(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// isTrackShaped checks for the three signature artifacts of a track folder.
func isTrackShaped(dir string) bool {
	if !isDir(dir) {
		return false
	}
	return isFile(filepath.Join(dir, specFile)) &&
		isFile(filepath.Join(dir, suiteFile)) &&
		isDir(filepath.Join(dir, trecDir))
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// isPath classifies a --judgments value: any separator or file extension means
// it's a path rather than a strategy name.
func isPath(s string) bool {
	if s == "" {
		return false
	}
	return filepath.IsAbs(s) || containsSeparator(s) || hasArtifactExtension(s)
}

func containsSeparator(s string) bool {
	for _, r := range s {
		if r == '/' || r == filepath.Separator {
			return true
		}
	}
	return false
}

func hasArtifactExtension(s string) bool {
	switch filepath.Ext(s) {
	case ".yaml", ".yml", ".tsv", ".json":
		return true
	default:
		return false
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
