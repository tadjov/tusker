package trackctx

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeTrack writes the three signature files that mark a folder as a track.
func makeTrack(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "trec"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "spec.yaml"), []byte(""), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "suite.yaml"), []byte(""), 0644))
}

// canonical returns the EvalSymlinks form of p — assertions about paths
// must compare against this since Resolve canonicalises its output.
func canonical(t *testing.T, p string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(p)
	require.NoError(t, err)
	return real
}

// chdir moves into dir for the duration of the test.
func chdir(t *testing.T, dir string) {
	t.Helper()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	require.NoError(t, os.Chdir(dir))
}

func TestResolve_RelativePathFromCWD(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "tracks", "news", "fts")
	makeTrack(t, track)
	chdir(t, dir)

	tr, err := Resolve(Inputs{TrackArg: "tracks/news/fts"})
	require.NoError(t, err)
	canonTrack := canonical(t, track)
	assert.Equal(t, canonTrack, tr.Root)
	assert.Equal(t, filepath.Join(canonTrack, "spec.yaml"), tr.Spec)
	assert.Equal(t, filepath.Join(canonTrack, "trec", "pool.yaml"), tr.Pool)
	assert.Equal(t, "tracks/news/fts", tr.Name(), "name is the path you typed")
}

func TestResolve_TrackRootShortensTheArg(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "tracks", "news", "fts")
	makeTrack(t, track)
	chdir(t, dir)

	tr, err := Resolve(Inputs{TrackArg: "fts", TrackRoot: "tracks/news"})
	require.NoError(t, err)
	assert.Equal(t, canonical(t, track), tr.Root)
	assert.Equal(t, "fts", tr.Name(), "name is root-relative")
}

func TestResolve_TrackRootAcceptsAnyLayout(t *testing.T) {
	// Nothing above the track folder is enforced: benches/b1 is as valid a
	// layout as tracks/<dataset>/<paradigm>.
	dir := t.TempDir()
	track := filepath.Join(dir, "benches", "b1")
	makeTrack(t, track)
	chdir(t, dir)

	tr, err := Resolve(Inputs{TrackArg: "b1", TrackRoot: "benches"})
	require.NoError(t, err)
	assert.Equal(t, canonical(t, track), tr.Root)
}

func TestResolve_AbsoluteArgIgnoresTrackRoot(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "elsewhere", "solo")
	makeTrack(t, track)

	tr, err := Resolve(Inputs{TrackArg: track, TrackRoot: "/does/not/exist"})
	require.NoError(t, err)
	assert.Equal(t, canonical(t, track), tr.Root)
	assert.Equal(t, "solo", tr.Name(), "outside the root, fall back to the base name")
}

func TestResolve_WalkUpFromNestedCWD(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "tracks", "demo")
	makeTrack(t, track)
	chdir(t, filepath.Join(track, "trec"))

	tr, err := Resolve(Inputs{})
	require.NoError(t, err)
	assert.Equal(t, canonical(t, track), tr.Root)
}

func TestResolve_ExplicitFlagsOverride(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "tracks", "demo")
	makeTrack(t, track)

	tr, err := Resolve(Inputs{
		TrackArg: track,
		SpecPath: "/abs/elsewhere/spec.yaml",
		PoolPath: "/abs/elsewhere/pool.yaml",
	})
	require.NoError(t, err)
	assert.Equal(t, "/abs/elsewhere/spec.yaml", tr.Spec)
	assert.Equal(t, "/abs/elsewhere/pool.yaml", tr.Pool)
}

func TestResolve_UnknownTrackErrors(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	_, err := Resolve(Inputs{TrackArg: "nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tried:", "error should name the path it tried")
	assert.Contains(t, err.Error(), "bench init nope", "error should suggest init command")
}

func TestResolve_ErrorSuggestsDeeperMatch(t *testing.T) {
	// The usual symptom of an unset or too-shallow track root: the folder
	// exists, just further down.
	dir := t.TempDir()
	makeTrack(t, filepath.Join(dir, "tracks", "news", "fts"))
	chdir(t, dir)

	_, err := Resolve(Inputs{TrackArg: "fts"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did you mean: tracks/news/fts")
	assert.Contains(t, err.Error(), "BENCH_TRACK_ROOT")
}

func TestResolve_NotTrackShapedErrors(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tracks", "news"), 0755))
	chdir(t, dir)

	_, err := Resolve(Inputs{TrackArg: "tracks/news"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestResolve_NoTrackNoWalkUp(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	_, err := Resolve(Inputs{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no track specified")
}

func TestResolveGlob_Expands(t *testing.T) {
	dir := t.TempDir()
	makeTrack(t, filepath.Join(dir, "tracks", "news", "fts"))
	makeTrack(t, filepath.Join(dir, "tracks", "news", "fuzzy"))
	makeTrack(t, filepath.Join(dir, "tracks", "wiki", "fts"))
	// A glob match that isn't track-shaped must be skipped.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tracks", "news", "scratch"), 0755))
	chdir(t, dir)

	tracks, err := ResolveGlob("tracks/news/*", "")
	require.NoError(t, err)

	var names []string
	for _, tr := range tracks {
		names = append(names, tr.Name())
	}
	assert.Equal(t, []string{"tracks/news/fts", "tracks/news/fuzzy"}, names, "track-shaped matches only, sorted")
}

func TestResolveGlob_RootScopesThePattern(t *testing.T) {
	dir := t.TempDir()
	makeTrack(t, filepath.Join(dir, "tracks", "news", "fts"))
	makeTrack(t, filepath.Join(dir, "tracks", "news", "fuzzy"))
	makeTrack(t, filepath.Join(dir, "tracks", "wiki", "fts"))
	chdir(t, dir)

	tracks, err := ResolveGlob("*", "tracks/news")
	require.NoError(t, err)

	var names []string
	for _, tr := range tracks {
		names = append(names, tr.Name())
	}
	assert.Equal(t, []string{"fts", "fuzzy"}, names, "'*' means every track in the root")
}

func TestResolveGlob_NoMatchErrors(t *testing.T) {
	dir := t.TempDir()
	makeTrack(t, filepath.Join(dir, "tracks", "news", "fts"))
	chdir(t, dir)

	_, err := ResolveGlob("tracks/wiki/*", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no tracks match pattern")
}

func TestIsPattern(t *testing.T) {
	assert.True(t, IsPattern("news/*"))
	assert.True(t, IsPattern("news/f?ts"))
	assert.False(t, IsPattern("news/fts"))
	assert.False(t, IsPattern("fts_quality"))
	assert.False(t, IsPattern(""))
}

func TestJudgmentsPath_StrategyVsExplicitPath(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "tracks", "demo")
	makeTrack(t, track)
	tr, err := Resolve(Inputs{TrackArg: track})
	require.NoError(t, err)

	t.Run("bare name expands to convention", func(t *testing.T) {
		assert.Equal(t,
			filepath.Join(canonical(t, track), "trec", "annotations.lexical.yaml"),
			tr.JudgmentsPath("lexical"))
	})
	t.Run("explicit path used verbatim", func(t *testing.T) {
		p := "/tmp/some/other.yaml"
		assert.Equal(t, p, tr.JudgmentsPath(p))
	})
	t.Run("empty returns empty", func(t *testing.T) {
		assert.Equal(t, "", tr.JudgmentsPath(""))
	})
}

func TestDefaultJudgmentsPath_RelativePathIsFromTheTrack(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "tracks", "demo")
	makeTrack(t, track)
	tr, err := Resolve(Inputs{TrackArg: track})
	require.NoError(t, err)
	canon := canonical(t, track)

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"relative path", "trec/annotations.hand.yaml", filepath.Join(canon, "trec", "annotations.hand.yaml")},
		{"absolute path", "/srv/grades/a.yaml", "/srv/grades/a.yaml"},
		{"bare name", "hand", filepath.Join(canon, "trec", "annotations.hand.yaml")},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tr.DefaultJudgmentsPath(tt.value))
		})
	}
}

// Suites resolve from the spec file's folder, so a judgments path in the same
// spec does too, even when --spec names a spec outside the track.
func TestDefaultJudgmentsPath_RelativePathIsFromTheSpecFolder(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "tracks", "demo")
	makeTrack(t, track)
	elsewhere := filepath.Join(dir, "specs", "variant.yaml")

	tr, err := Resolve(Inputs{TrackArg: track, SpecPath: elsewhere})
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(dir, "specs", "trec", "annotations.hand.yaml"),
		tr.DefaultJudgmentsPath("trec/annotations.hand.yaml"))
}

func TestQrelsPath_StrategySuffix(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "tracks", "demo")
	makeTrack(t, track)
	tr, _ := Resolve(Inputs{TrackArg: track})
	canon := canonical(t, track)
	assert.Equal(t,
		filepath.Join(canon, "trec", "qrels.claude-api.tsv"),
		tr.QrelsPath("claude-api"))
}

func TestQrelsPath_AnnotationsPathMapsToQrelsBesideIt(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "tracks", "demo")
	makeTrack(t, track)
	tr, err := Resolve(Inputs{TrackArg: track})
	require.NoError(t, err)

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"annotations yaml", "trec/annotations.claude-cli.yaml", "trec/qrels.claude-cli.tsv"},
		{"any yml", "/tmp/grades/hand.yml", "/tmp/grades/qrels.hand.tsv"},
		{"tsv path used verbatim", "/tmp/out/q.tsv", "/tmp/out/q.tsv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tr.QrelsPath(tt.value))
		})
	}
}

func TestReportPath_UsesRunID(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "tracks", "demo")
	makeTrack(t, track)
	tr, _ := Resolve(Inputs{TrackArg: track})
	canon := canonical(t, track)
	assert.Equal(t,
		filepath.Join(canon, "reports", "2026-05-21T10-00-00-run-abc123.json"),
		tr.ReportPath("2026-05-21T10-00-00-run-abc123"))
	assert.Equal(t,
		filepath.Join(canon, "reports", "latest.json"),
		tr.LatestReportPath())
}
