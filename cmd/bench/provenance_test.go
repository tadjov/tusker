package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/report"
	"github.com/tadjov/tusker/internal/storage/pg"
)

func readFixtureReport(t *testing.T, name string) *report.Report {
	t.Helper()
	rpt, err := report.ReadJSON(filepath.Join("fixtures", "provenance", name))
	require.NoError(t, err)
	return rpt
}

func provenanceLine(engineName, field, before, after string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(engineName) + `\s+` + regexp.QuoteMeta(field) +
		`\s+` + regexp.QuoteMeta(before) + `\s+` + regexp.QuoteMeta(after) + `\s*$`)
}

func TestPrintDiff_ListsChangedProvenanceBeforeMetricDeltas(t *testing.T) {
	var out bytes.Buffer
	printDiff(&out, readFixtureReport(t, "before.json"), readFixtureReport(t, "after.json"))
	got := out.String()

	assert.Regexp(t, provenanceLine("pg-native", "setting.hnsw.ef_search", "100", "200"), got)
	assert.Regexp(t, provenanceLine("es", "version", "8.12.0", "8.15.0"), got)
	assert.Regexp(t, `(?m)^es\s+analysis\s+sha256:\w+\s+sha256:\w+\s*$`, got)

	assert.NotContains(t, got, "postgresenglishstopword", "the analysis block is shown as a digest, never in full")
	for _, unchanged := range []string{"extension.vector", "setting.plan_cache_mode", "field.title", "server_version"} {
		assert.NotContains(t, got, unchanged, "only fields that differ are listed")
	}

	provenanceAt := strings.Index(got, "Index provenance changes")
	firstJobAt := strings.Index(got, "--- Job:")
	require.NotEqual(t, -1, provenanceAt)
	require.NotEqual(t, -1, firstJobAt)
	assert.Less(t, provenanceAt, firstJobAt)
}

func TestPrintDiff_ReportWithoutProvenanceReadsAsNotRecorded(t *testing.T) {
	var out bytes.Buffer
	printDiff(&out, readFixtureReport(t, "legacy.json"), readFixtureReport(t, "after.json"))
	got := out.String()

	assert.Regexp(t, provenanceLine("pg-native", "provenance", "not recorded", "recorded"), got)
	assert.Regexp(t, provenanceLine("es", "provenance", "not recorded", "recorded"), got)
	assert.NotContains(t, got, "setting.hnsw.ef_search", "an unrecorded side is one line, not a field-by-field flood")
}

func TestPrintDiff_SameProvenanceSaysUnchanged(t *testing.T) {
	var out bytes.Buffer
	printDiff(&out, readFixtureReport(t, "after.json"), readFixtureReport(t, "after.json"))

	assert.Contains(t, out.String(), "Index provenance: unchanged")
}

func TestShowReport_SummarisesEachEngineIndex(t *testing.T) {
	var out bytes.Buffer
	showReport(&out, readFixtureReport(t, "after.json"))
	got := out.String()

	assert.Regexp(t, `(?m)^\s+pg-native\s+postgres\s+postgres 18\.0.*vector 0\.8\.1.*hnsw\.ef_search=200`, got)
	assert.Regexp(t, `(?m)^\s+es\s+elasticsearch\s+elasticsearch 8\.15\.0.*index: news-cc-000001.*analyzers: multilingual_analyzer`, got)
	assert.Regexp(t, `(?m)^\s+api\s+api\s+error: engine cannot describe its index`, got)
	assert.NotContains(t, got, "postgresenglishstopword")
}

func TestShowReport_ReportWithoutProvenanceStillShows(t *testing.T) {
	var out bytes.Buffer
	showReport(&out, readFixtureReport(t, "legacy.json"))

	assert.Regexp(t, `(?m)^\s+pg-native\s+postgres\s+index provenance not recorded`, out.String())
}

type describedExecutor struct {
	description *engine.IndexDescription
	err         error
}

func (e describedExecutor) Execute(context.Context, engine.Request) (*engine.Execution, error) {
	return &engine.Execution{}, nil
}
func (e describedExecutor) Name() string { return "described" }
func (e describedExecutor) Close() error { return nil }
func (e describedExecutor) DescribeIndex(context.Context) (*engine.IndexDescription, error) {
	return e.description, e.err
}

type undescribedExecutor struct{}

func (undescribedExecutor) Execute(context.Context, engine.Request) (*engine.Execution, error) {
	return &engine.Execution{}, nil
}
func (undescribedExecutor) Name() string { return "undescribed" }
func (undescribedExecutor) Close() error { return nil }

func TestCollectIndexProvenance_RecordsFailuresWithoutFailing(t *testing.T) {
	var warnings bytes.Buffer
	provenance := collectIndexProvenance(context.Background(), &warnings, map[string]engine.Executor{
		"pg-native": describedExecutor{description: &engine.IndexDescription{
			Postgres: &pg.IndexDescription{ServerVersion: "18.0"},
		}},
		"es":  describedExecutor{err: errors.New("connection refused")},
		"api": undescribedExecutor{},
	})

	require.Contains(t, provenance, "pg-native")
	assert.Equal(t, "18.0", provenance["pg-native"].Version())
	assert.Empty(t, provenance["pg-native"].Error)

	require.Contains(t, provenance, "es")
	assert.Contains(t, provenance["es"].Error, "connection refused")
	assert.Contains(t, warnings.String(), "connection refused")

	require.Contains(t, provenance, "api", "an engine that cannot describe its index still gets a block")
	assert.NotEmpty(t, provenance["api"].Error)
}
