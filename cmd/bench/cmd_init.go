package main

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

//go:embed templates/*.tmpl
var initTemplates embed.FS

type initFlags struct {
	force bool
}

type initContext struct {
	Name           string
	SuiteRel       string
	AnnotationsRel string
}

func newInitCmd() *cobra.Command {
	var f initFlags
	cmd := &cobra.Command{
		Use:   "init <track>",
		Short: "Scaffold a new evaluation track folder",
		Long: `Creates a self-contained evaluation track at the given path:

  <track>/
    spec.yaml          # engines + jobs + defaults
    suite.yaml         # query templates + queries
    trec/              # generated pool, annotations, qrels live here
    reports/           # one JSON per bench run
    README.md          # workflow notes

The folder IS the track — no hidden state, no selector, and no directory
layout above it is assumed. The path is relative to --track-root
(BENCH_TRACK_ROOT, default: current directory) unless it is absolute.

Group related tracks by putting them in one folder and running a glob:
  bench init tracks/cc-news/fts_quality
  bench run 'tracks/cc-news/*'`,
		Example: "  bench init tracks/cc-news/fts_quality\n  BENCH_TRACK_ROOT=tracks/cc-news bench init news_fuzzy",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return executeInit(cmd, f, args[0])
		},
	}
	cmd.Flags().BoolVar(&f.force, "force", false, "Overwrite existing files in the track folder")
	return cmd
}

func executeInit(cmd *cobra.Command, f initFlags, name string) error {
	if err := validateTrackPath(name); err != nil {
		return err
	}

	// The arg is a path like any other: absolute as-is, relative against the
	// track root (default: cwd). No parent layout is invented.
	root := name
	if !filepath.IsAbs(name) {
		root = filepath.Join(trackRootOrCwd(), name)
	}

	if info, err := os.Stat(root); err == nil && info.IsDir() && !f.force {
		entries, _ := os.ReadDir(root)
		if len(entries) > 0 {
			return fmt.Errorf("track %q already exists and is non-empty (use --force to overwrite)", root)
		}
	}
	for _, sub := range []string{"trec", "reports"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Join(root, sub), err)
		}
	}

	ctx := initContext{
		Name:           filepath.Base(root),
		SuiteRel:       "suite.yaml",
		AnnotationsRel: filepath.Join("trec", "annotations.keyword.yaml"),
	}

	files := []struct {
		tmpl string
		dest string
	}{
		{"templates/spec.yaml.tmpl", filepath.Join(root, "spec.yaml")},
		{"templates/suite.yaml.tmpl", filepath.Join(root, "suite.yaml")},
		{"templates/README.md.tmpl", filepath.Join(root, "README.md")},
	}

	for _, file := range files {
		if err := renderTemplate(file.tmpl, file.dest, ctx, f.force); err != nil {
			return err
		}
	}

	_ = os.WriteFile(filepath.Join(root, "trec", ".gitkeep"), nil, 0644)
	_ = os.WriteFile(filepath.Join(root, "reports", ".gitkeep"), nil, 0644)

	// The arg as typed addresses the track again under the same root, so it is
	// the hint that always works.
	cmd.Printf("Track created: %s/\n", root)
	cmd.Printf("Next: edit %s/suite.yaml, then run:\n", root)
	cmd.Printf("  bench validate %s\n", name)
	return nil
}

func renderTemplate(srcPath, destPath string, ctx initContext, force bool) error {
	if _, err := os.Stat(destPath); err == nil && !force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", destPath)
	}
	raw, err := initTemplates.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read embedded template %s: %w", srcPath, err)
	}
	tmpl, err := template.New(filepath.Base(srcPath)).Parse(string(raw))
	if err != nil {
		return fmt.Errorf("parse template %s: %w", srcPath, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return fmt.Errorf("execute template %s: %w", srcPath, err)
	}
	if err := os.WriteFile(destPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("write %s: %w", destPath, err)
	}
	return nil
}

// validateTrackPath guards the scaffold target. It is an ordinary path —
// absolute or relative, nested as deep as you like — so only empty segments and
// characters that make a path awkward to type back are rejected.
func validateTrackPath(p string) error {
	if p == "" {
		return fmt.Errorf("track path is empty")
	}
	if strings.HasSuffix(p, "/") || strings.Contains(p, "//") {
		return fmt.Errorf("track path has an empty path segment: %q", p)
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_' || r == '/' || r == '.':
		default:
			return fmt.Errorf("track path may only contain [a-zA-Z0-9_-.] and /: %q", p)
		}
	}
	return nil
}
