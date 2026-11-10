package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/storage/es"
)

// IndexProvenance is how an engine had the corpus indexed when the run
// started. Error is set instead of a description when it could not be read;
// a run never fails over provenance.
type IndexProvenance struct {
	engine.IndexDescription
	Error string `json:"error,omitempty"`
}

// ProvenanceField is one comparable fact from an IndexProvenance.
type ProvenanceField struct {
	Name  string
	Value string
}

// ProvenanceChange is a provenance field whose value differs between two
// reports of the same engine. Before or After is empty when the field exists
// on one side only.
type ProvenanceChange struct {
	Engine string
	Field  string
	Before string
	After  string
}

const notRecorded = "not recorded"

// Fields flattens the provenance into named values sorted by name. The ES
// analysis block is reduced to a digest so a change reads as one field.
func (p IndexProvenance) Fields() []ProvenanceField {
	var fields []ProvenanceField
	add := func(name, value string) {
		fields = append(fields, ProvenanceField{Name: name, Value: value})
	}
	if p.Error != "" {
		add("error", p.Error)
	}
	if d := p.Postgres; d != nil {
		add("server_version", d.ServerVersion)
		for _, ext := range d.Extensions {
			add("extension."+ext.Name, ext.Version)
		}
		// pg_get_indexdef renders reloptions in its WITH clause, so the
		// definition alone carries both.
		for _, index := range d.Indexes {
			add("index."+index.Table+"."+index.Name, index.Definition)
		}
		for name, value := range d.Settings {
			add("setting."+name, value)
		}
		addLoadFields(add, d.Load)
	}
	if d := p.Elasticsearch; d != nil {
		add("version", d.Version)
		add("index", d.Index)
		if len(d.Analysis) > 0 {
			add("analysis", digest(d.Analysis))
		}
		for path, field := range d.Fields {
			add("field."+path, describeField(field))
		}
		addLoadFields(add, d.Load)
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	return fields
}

// Summary is a one-line account of the provenance for `bench show report`.
func (p IndexProvenance) Summary() string {
	var parts []string
	if d := p.Postgres; d != nil {
		parts = append(parts, "postgres "+d.ServerVersion)
		if len(d.Extensions) > 0 {
			extensions := make([]string, len(d.Extensions))
			for i, ext := range d.Extensions {
				extensions[i] = ext.Name + " " + ext.Version
			}
			parts = append(parts, "extensions: "+strings.Join(extensions, ", "))
		}
		parts = append(parts, fmt.Sprintf("indexes: %d", len(d.Indexes)))
		if len(d.Settings) > 0 {
			parts = append(parts, "settings: "+formatSettings(d.Settings))
		}
		parts = appendLoadSummary(parts, d.Load)
	}
	if d := p.Elasticsearch; d != nil {
		parts = append(parts, "elasticsearch "+d.Version, "index: "+d.Index)
		if analyzers := analyzerNames(d.Analysis); len(analyzers) > 0 {
			parts = append(parts, "analyzers: "+strings.Join(analyzers, ", "))
		}
		parts = append(parts, fmt.Sprintf("fields: %d", len(d.Fields)))
		parts = appendLoadSummary(parts, d.Load)
	}
	if p.Error != "" {
		parts = append(parts, "error: "+p.Error)
	}
	return strings.Join(parts, " · ")
}

// DiffProvenance lists, engine by engine, the provenance fields that differ
// between two reports. An engine whose provenance one report did not record
// yields a single change rather than one per field.
func DiffProvenance(before, after *Report) []ProvenanceChange {
	var changes []ProvenanceChange
	for _, name := range sharedEngines(before, after) {
		a := before.Environment.Engines[name].Index
		b := after.Environment.Engines[name].Index
		switch {
		case a == nil && b == nil:
			continue
		case a == nil:
			changes = append(changes, ProvenanceChange{Engine: name, Field: "provenance", Before: notRecorded, After: "recorded"})
		case b == nil:
			changes = append(changes, ProvenanceChange{Engine: name, Field: "provenance", Before: "recorded", After: notRecorded})
		default:
			changes = append(changes, diffFields(name, a.Fields(), b.Fields())...)
		}
	}
	return changes
}

func addLoadFields(add func(name, value string), load *storage.LoadRecord) {
	if load == nil {
		return
	}
	add("load.canonical.version", load.Canonical.Version)
	add("load.embeddings.version", embeddingsVersion(load))
	add("load.model_tag", load.ModelTag)
}

func appendLoadSummary(parts []string, load *storage.LoadRecord) []string {
	if load == nil {
		return append(parts, "load: "+notRecorded)
	}
	return append(parts, fmt.Sprintf("load: %s + %s (%s)", load.Canonical.Version, embeddingsVersion(load), load.ModelTag))
}

func embeddingsVersion(load *storage.LoadRecord) string {
	if load.Embeddings == nil {
		return "no embeddings"
	}
	return load.Embeddings.Version
}

func sharedEngines(a, b *Report) []string {
	var names []string
	for name := range a.Environment.Engines {
		if _, ok := b.Environment.Engines[name]; ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func diffFields(engineName string, before, after []ProvenanceField) []ProvenanceChange {
	beforeValues := make(map[string]string, len(before))
	names := make(map[string]struct{}, len(before)+len(after))
	for _, f := range before {
		beforeValues[f.Name] = f.Value
		names[f.Name] = struct{}{}
	}
	afterValues := make(map[string]string, len(after))
	for _, f := range after {
		afterValues[f.Name] = f.Value
		names[f.Name] = struct{}{}
	}

	var changes []ProvenanceChange
	for name := range names {
		if beforeValues[name] != afterValues[name] {
			changes = append(changes, ProvenanceChange{
				Engine: engineName,
				Field:  name,
				Before: beforeValues[name],
				After:  afterValues[name],
			})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Field < changes[j].Field })
	return changes
}

func describeField(f es.FieldMapping) string {
	parts := []string{f.Type}
	appendSet := func(key, value string) {
		if value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	appendSet("analyzer", f.Analyzer)
	appendSet("search_analyzer", f.SearchAnalyzer)
	appendSet("index_options", f.IndexOptions)
	if f.Dims > 0 {
		appendSet("dims", fmt.Sprint(f.Dims))
	}
	appendSet("similarity", f.Similarity)
	if v := f.VectorIndex; v != nil {
		appendSet("index", v.Type)
		appendSet("m", fmt.Sprint(v.M))
		appendSet("ef_construction", fmt.Sprint(v.EfConstruction))
	}
	return strings.Join(parts, " ")
}

// digest ignores whitespace, which a report read back from indented JSON
// carries inside its raw analysis block.
func digest(raw json.RawMessage) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		compact.Reset()
		compact.Write(raw)
	}
	sum := sha256.Sum256(compact.Bytes())
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

func analyzerNames(analysis json.RawMessage) []string {
	var parsed struct {
		Analyzer map[string]json.RawMessage `json:"analyzer"`
	}
	if err := json.Unmarshal(analysis, &parsed); err != nil {
		return nil
	}
	names := make([]string, 0, len(parsed.Analyzer))
	for name := range parsed.Analyzer {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func formatSettings(settings map[string]string) string {
	names := make([]string, 0, len(settings))
	for name := range settings {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, len(names))
	for i, name := range names {
		pairs[i] = name + "=" + settings[name]
	}
	return strings.Join(pairs, " ")
}
